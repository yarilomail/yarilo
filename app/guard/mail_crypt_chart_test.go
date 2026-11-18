package guard_test

import (
	"os/exec"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type podWorkload struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Name string `yaml:"name"`
					Env  []struct {
						Name  string `yaml:"name"`
						Value string `yaml:"value"`
					} `yaml:"env"`
					VolumeMounts []struct {
						Name      string `yaml:"name"`
						MountPath string `yaml:"mountPath"`
					} `yaml:"volumeMounts"`
				} `yaml:"containers"`
				Volumes []struct {
					Name     string         `yaml:"name"`
					EmptyDir map[string]any `yaml:"emptyDir"`
				} `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

func renderWorkloads(t *testing.T, args ...string) []podWorkload {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "helm", append([]string{"template", "t", "../../helm"}, args...)...).Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	var ws []podWorkload
	for _, doc := range strings.Split(string(out), "\n---\n") {
		var w podWorkload
		if yaml.Unmarshal([]byte(doc), &w) == nil && (w.Kind == "Deployment" || w.Kind == "StatefulSet") {
			ws = append(ws, w)
		}
	}
	return ws
}

// The mail encryption keys reach every container that opens mail and no
// other; /tmp is bounded in every pod by tmpSizeLimit, 320Mi when the values
// leave it alone, and no key is mounted when mailCrypt is not set.
func TestMailCryptKeysAndTmpLimitInTheChart(t *testing.T) {
	opensMail := map[string]bool{"yarilo-imap": true, "yarilo-pop3": true, "yarilo-lmtp": true, "yarilo-jmap": true,
		"yarilo-managesieve": true, "yarilo-fts": true, "yarilo-backend-api": true}
	// The co-located StatefulSet (the stand) and the per-protocol Deployments.
	ws := renderWorkloads(t, "-f", "../../helm_values/values-sandbox.yaml",
		"--set", "mailCrypt.secretName=keys", "--set", "tmpSizeLimit=1Gi")
	ws = append(ws, renderWorkloads(t, "--set", "mailCrypt.secretName=keys", "--set", "tmpSizeLimit=1Gi",
		"--set", "components.backend.coLocated=false", "--set", "components.fts.enabled=true",
		"--set", "components.submission.enabled=true")...)
	keyed := 0
	for _, w := range ws {
		for _, c := range w.Spec.Template.Spec.Containers {
			has := false
			for _, m := range c.VolumeMounts {
				if m.Name == "mail-crypt" && m.MountPath == "/etc/yarilo/mail-crypt" {
					has = true
				}
			}
			if has != opensMail[c.Name] {
				t.Errorf("%s/%s: mounts the keys %v, opens mail %v", w.Metadata.Name, c.Name, has, opensMail[c.Name])
			}
			if has {
				keyed++
			}
		}
		names := map[string]int{}
		for _, v := range w.Spec.Template.Spec.Volumes {
			if names[v.Name]++; names[v.Name] == 2 {
				t.Errorf("%s: volume %q twice: the API server refuses the pod spec", w.Metadata.Name, v.Name)
			}
			if v.Name == "tmp" && v.EmptyDir["sizeLimit"] != "1Gi" {
				t.Errorf("%s: /tmp %v, want sizeLimit 1Gi", w.Metadata.Name, v.EmptyDir)
			}
		}
	}
	if keyed == 0 {
		t.Fatal("no container mounts the keys")
	}
	for _, w := range renderWorkloads(t, "-f", "../../helm_values/values-sandbox.yaml") {
		for _, v := range w.Spec.Template.Spec.Volumes {
			if v.Name == "mail-crypt" {
				t.Errorf("%s: mail-crypt mounted without mailCrypt set", w.Metadata.Name)
			}
			if v.Name == "tmp" && v.EmptyDir["sizeLimit"] != "320Mi" {
				t.Errorf("%s: /tmp %v by default, want sizeLimit 320Mi", w.Metadata.Name, v.EmptyDir)
			}
		}
	}
}
