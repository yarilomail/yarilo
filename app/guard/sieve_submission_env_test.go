package guard_test

import (
	"os/exec"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type envVar struct {
	Name      string `yaml:"name"`
	ValueFrom struct {
		SecretKeyRef struct {
			Name string `yaml:"name"`
			Key  string `yaml:"key"`
		} `yaml:"secretKeyRef"`
	} `yaml:"valueFrom"`
}

// sieveSenderEnv renders the chart and returns, per container that runs the
// Sieve sender, the secret and key each submission variable comes from.
func sieveSenderEnv(t *testing.T, args ...string) map[string]map[string]string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "helm", append([]string{"template", "yarilo", "../../helm", "-f", "../../helm_values/values-sandbox.yaml"}, args...)...).Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	var doc struct {
		Kind string `yaml:"kind"`
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name string   `yaml:"name"`
						Env  []envVar `yaml:"env"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	got := map[string]map[string]string{}
	dec := yaml.NewDecoder(strings.NewReader(string(out)))
	for dec.Decode(&doc) == nil {
		for _, c := range doc.Spec.Template.Spec.Containers {
			if c.Name != "yarilo-lmtp" && c.Name != "yarilo-imap" {
				continue
			}
			vars := map[string]string{}
			for _, e := range c.Env {
				if strings.HasPrefix(e.Name, "YARILO_SIEVE_SUBMISSION_") {
					vars[e.Name] = e.ValueFrom.SecretKeyRef.Name + "/" + e.ValueFrom.SecretKeyRef.Key
				}
			}
			got[doc.Kind+"/"+c.Name] = vars
		}
		doc.Spec.Template.Spec.Containers = nil
	}
	return got
}

// Every container that runs the Sieve sender (LMTP, and IMAP for imapsieve)
// logs in with sieve.sieve_submission_auth_secret, in the co-located
// StatefulSet and in the per-protocol Deployments.
func TestSieveSenderLogsInWhereverItRuns(t *testing.T) {
	want := map[string]string{"YARILO_SIEVE_SUBMISSION_USER": "sieve-smtp-auth/user", "YARILO_SIEVE_SUBMISSION_PASSWORD": "sieve-smtp-auth/password"}
	for _, tc := range []struct {
		name       string
		args       []string
		containers []string
	}{
		{"co-located", nil, []string{"StatefulSet/yarilo-lmtp", "StatefulSet/yarilo-imap"}},
		{"per protocol", []string{"--set", "components.backend.coLocated=false"}, []string{"Deployment/yarilo-lmtp", "Deployment/yarilo-imap"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sieveSenderEnv(t, tc.args...)
			for _, c := range tc.containers {
				vars, ok := got[c]
				if !ok {
					t.Errorf("%s not rendered", c)
					continue
				}
				for k, v := range want {
					if vars[k] != v {
						t.Errorf("%s: %s from %q, want %q", c, k, vars[k], v)
					}
				}
			}
		})
	}
}

// The key the chart used to read for the env is refused, not dropped.
func TestTheOldSieveAuthSecretKeyIsRefused(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "helm", "template", "yarilo", "../../helm", "-f", "../../helm_values/values-sandbox.yaml",
		"--set", "sieve.submission.authSecret=sieve-smtp-auth").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "set sieve.sieve_submission_auth_secret") {
		t.Errorf("helm template with sieve.submission.authSecret: %v\n%s", err, out)
	}
}

// An explicit false and the CA path reach the ConfigMap as written.
func TestSieveSubmissionTLSReachesTheConfig(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "helm", "template", "yarilo", "../../helm", "-f", "../../helm_values/values-sandbox.yaml",
		"--set", "sieve.sieve_submission_ssl_verify=false").Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	for _, line := range []string{"sieve_submission_ssl_verify: false", `sieve_submission_ssl_ca: "/etc/yarilo/internal-tls/ca.crt"`} {
		if !strings.Contains(string(out), line) {
			t.Errorf("the ConfigMap has no %q", line)
		}
	}
}
