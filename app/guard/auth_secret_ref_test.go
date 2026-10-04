package guard_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

var authSecretEnv = []string{"YARILO_OAUTH2_0_CLIENT_SECRET", "YARILO_AUTH_POLICY_API_HEADER", "YARILO_AUTH_POLICY_HASH_NONCE"}

// renderAuth renders the chart with one oauth2 entry and the policy credentials;
// lit and ref name the fields given as a literal and as a Secret reference.
func renderAuth(t *testing.T, lit, ref map[string]bool) ([]byte, error) {
	t.Helper()
	v := "components:\n  auth:\n    oauth2:\n" +
		"      - name: idp\n        introspection_url: https://idp.example/introspect\n        client_id: yarilo-auth\n"
	if lit["client_secret"] {
		v += "        client_secret: lit-client-secret\n"
	}
	if ref["client_secret"] {
		v += "        client_secret_ref: {name: oauth-s, key: cs}\n"
	}
	v += "    policy:\n      url: https://policy.example\n"
	if lit["api_header"] {
		v += "      api_header: \"X-API-Key: lit-header\"\n"
	}
	if ref["api_header"] {
		v += "      api_header_secret_ref: {name: policy-s, key: ah}\n"
	}
	if lit["hash_nonce"] {
		v += "      hash_nonce: lit-nonce\n"
	}
	if ref["hash_nonce"] {
		v += "      hash_nonce_secret_ref: {name: policy-s, key: hn}\n"
	}
	f := filepath.Join(t.TempDir(), "auth.yaml")
	if err := os.WriteFile(f, []byte(v), 0o600); err != nil {
		t.Fatal(err)
	}
	return exec.Command("helm", "template", "yarilo", "../../helm", "-f", "../../helm_values/values-sandbox.yaml", "-f", f).CombinedOutput()
}

// authEnv returns, per component, the env names that come from a Secret.
func authEnv(t *testing.T, out []byte) map[string][]string {
	t.Helper()
	type env struct {
		Name      string `yaml:"name"`
		Value     string `yaml:"value"`
		ValueFrom *struct {
			SecretKeyRef *struct{} `yaml:"secretKeyRef"`
		} `yaml:"valueFrom"`
	}
	var doc struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Env []env `yaml:"env"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	got := map[string][]string{}
	dec := yaml.NewDecoder(strings.NewReader(string(out)))
	for dec.Decode(&doc) == nil {
		for _, c := range doc.Spec.Template.Spec.Containers {
			component := ""
			var fromSecret []string
			for _, e := range c.Env {
				if e.Name == "YARILO_COMPONENT" {
					component = e.Value
				}
				if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
					fromSecret = append(fromSecret, e.Name)
				}
			}
			got[component] = append(got[component], fromSecret...)
		}
		doc.Spec.Template.Spec.Containers = nil
	}
	return got
}

// With a *_secret_ref the config names ${YARILO_…} and only yarilo-auth gets the
// variable; a literal renders as before; both fail the render (#2163).
func TestAuthCredentialsComeFromTheSecret(t *testing.T) {
	all := map[string]bool{"client_secret": true, "api_header": true, "hash_nonce": true}
	cases := []struct {
		name            string
		lit, ref        map[string]bool
		wantEnvInConfig bool
		wantLiteral     bool
		bothSet         string // the key a render with both must name
	}{
		{name: "secret refs", ref: all, wantEnvInConfig: true},
		{name: "literals", lit: all, wantLiteral: true},
		{name: "client_secret both", lit: map[string]bool{"client_secret": true}, ref: map[string]bool{"client_secret": true}, bothSet: "client_secret"},
		{name: "api_header both", lit: map[string]bool{"api_header": true}, ref: map[string]bool{"api_header": true}, bothSet: "api_header"},
		{name: "hash_nonce both", lit: map[string]bool{"hash_nonce": true}, ref: map[string]bool{"hash_nonce": true}, bothSet: "hash_nonce"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := renderAuth(t, tc.lit, tc.ref)
			if tc.bothSet != "" {
				if err == nil {
					t.Fatalf("render succeeded with %s and its secret_ref both set", tc.bothSet)
				}
				if !strings.Contains(string(out), tc.bothSet+"_secret_ref") {
					t.Errorf("render failure does not name %s_secret_ref: %s", tc.bothSet, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("helm template: %v\n%s", err, out)
			}
			for _, v := range authSecretEnv {
				if got := strings.Contains(string(out), "${"+v+"}"); got != tc.wantEnvInConfig {
					t.Errorf("${%s} in the config = %v, want %v", v, got, tc.wantEnvInConfig)
				}
			}
			for _, lit := range []string{"lit-client-secret", "lit-header", "lit-nonce"} {
				if got := strings.Contains(string(out), lit); got != tc.wantLiteral {
					t.Errorf("literal %q rendered = %v, want %v", lit, got, tc.wantLiteral)
				}
			}
			seen := false
			for component, names := range authEnv(t, out) {
				seen = seen || component == "yarilo-auth"
				for _, v := range authSecretEnv {
					has := strings.Contains(strings.Join(names, " "), v)
					if want := tc.ref != nil && component == "yarilo-auth"; has != want {
						t.Errorf("%s has %s from a Secret = %v, want %v", component, v, has, want)
					}
				}
			}
			if !seen {
				t.Fatal("no yarilo-auth container rendered; the guard looks at nothing")
			}
		})
	}
}
