package guard_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

func renderOAuth2(t *testing.T, entry string) ([]byte, error) {
	t.Helper()
	f := filepath.Join(t.TempDir(), "oauth2.yaml")
	if err := os.WriteFile(f, []byte("components:\n  auth:\n    oauth2:\n"+entry), 0o600); err != nil {
		t.Fatal(err)
	}
	return exec.Command("helm", "template", "yarilo", "../../helm", "-f", "../../helm_values/values-sandbox.yaml", "-f", f).CombinedOutput()
}

// A local key given as secret_ref is mounted into yarilo-auth at the path the
// rendered config names; a file is passed through as written (#2231).
func TestOAuth2LocalKeySecretReachesTheAuthPod(t *testing.T) {
	out, err := renderOAuth2(t, `      - mode: local
        grant_url: https://idp.example/token
        client_id: yarilo-auth
        force_introspection: true
        tls_ca: /etc/yarilo/internal-tls/ca.crt
        local_keys:
          - {alg: HS256, kid: sb-hs256, secret_ref: {name: oauth-keys, key: hs256}}
          - {alg: RS256, azp: app, file: /keys/rs.pem}
`)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	path := filepath.Join(t.TempDir(), "yarilo.yaml")
	if err := os.WriteFile(path, []byte(renderedConfig(t, string(out))), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	e := cfg.Auth.OAuth2[0]
	if e.GrantURL != "https://idp.example/token" || !e.ForceIntrospection || e.TLSCA != "/etc/yarilo/internal-tls/ca.crt" {
		t.Errorf("grant %q force %v tls_ca %q did not reach the config", e.GrantURL, e.ForceIntrospection, e.TLSCA)
	}
	if len(e.LocalKeys) != 2 || e.LocalKeys[1].File != "/keys/rs.pem" || e.LocalKeys[1].Azp != "app" || e.LocalKeys[0].Kid != "sb-hs256" {
		t.Fatalf("local keys = %+v", e.LocalKeys)
	}
	mounted := e.LocalKeys[0].File

	type workload struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Mounts []struct {
							Name      string `yaml:"name"`
							MountPath string `yaml:"mountPath"`
						} `yaml:"volumeMounts"`
					} `yaml:"containers"`
					Volumes []struct {
						Name   string `yaml:"name"`
						Secret *struct {
							SecretName string `yaml:"secretName"`
							Items      []struct {
								Key, Path string
							} `yaml:"items"`
						} `yaml:"secret"`
					} `yaml:"volumes"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	found := 0
	dec := yaml.NewDecoder(strings.NewReader(string(out)))
	for {
		var w workload
		if dec.Decode(&w) != nil {
			break
		}
		for _, c := range w.Spec.Template.Spec.Containers {
			for _, m := range c.Mounts {
				if m.Name != "oauth2-key-0-0" {
					continue
				}
				if w.Metadata.Name != "yarilo-auth" {
					t.Errorf("%s %s mounts the OAuth2 key; only yarilo-auth verifies tokens", w.Kind, w.Metadata.Name)
				}
				if m.MountPath+"/key" != mounted {
					t.Errorf("mounted at %s, config reads %s", m.MountPath, mounted)
				}
				found++
			}
		}
		for _, v := range w.Spec.Template.Spec.Volumes {
			if v.Name == "oauth2-key-0-0" && (v.Secret == nil || v.Secret.SecretName != "oauth-keys" ||
				len(v.Secret.Items) != 1 || v.Secret.Items[0].Key != "hs256" || v.Secret.Items[0].Path != "key") {
				t.Errorf("volume %+v does not project oauth-keys/hs256 as key", v.Secret)
			}
		}
	}
	if found != 1 {
		t.Fatalf("the key is mounted %d times, want once (yarilo-auth)", found)
	}
}

// A local key with both sources, or neither, is refused at render.
func TestOAuth2LocalKeyNeedsOneSource(t *testing.T) {
	for name, key := range map[string]string{
		"both":    `{alg: RS256, file: /k, secret_ref: {name: s, key: k}}`,
		"neither": `{alg: RS256}`,
	} {
		t.Run(name, func(t *testing.T) {
			out, err := renderOAuth2(t, "      - mode: local\n        local_keys:\n          - "+key+"\n")
			if err == nil {
				t.Fatalf("rendered a key with %s source", name)
			}
			if !strings.Contains(string(out), "local_keys[0]") {
				t.Errorf("the refusal does not name the key:\n%s", out)
			}
		})
	}
}
