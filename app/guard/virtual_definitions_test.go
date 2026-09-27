package guard_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// A shared virtual namespace is only usable when its definitions are mounted
// where its mail_path names them, in the containers that read mailboxes.
func TestSharedVirtualDefinitionsReachTheContainersThatReadThem(t *testing.T) {
	values := `
virtualDefinitions:
  All: |
    *
    -Trash
namespaces:
  - type: personal
    prefix: ""
    separator: "/"
    list: "yes"
    inbox: true
  - type: shared
    prefix: "Virtual/"
    separator: "/"
    hidden: true
    list: "no"
    subscriptions: false
    mail_driver: virtual
    mail_path: /etc/yarilo/virtual
    mail_index_path: "%h/index/virtual"
`
	f, err := os.CreateTemp(t.TempDir(), "values-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(values); err != nil {
		t.Fatal(err)
	}
	f.Close() //nolint:errcheck
	out, err := exec.Command("helm", "template", "../../helm",
		"-f", "../../helm_values/values-sandbox.yaml", "-f", f.Name()).Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}

	mounted := map[string]bool{}
	definitions := false
	for _, doc := range strings.Split(string(out), "\n---\n") {
		var obj struct {
			Kind     string            `yaml:"kind"`
			Data     map[string]string `yaml:"data"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Name         string `yaml:"name"`
							VolumeMounts []struct {
								Name      string `yaml:"name"`
								MountPath string `yaml:"mountPath"`
								ReadOnly  bool   `yaml:"readOnly"`
							} `yaml:"volumeMounts"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("parse chart output: %v", err)
		}
		if obj.Kind == "ConfigMap" && strings.HasSuffix(obj.Metadata.Name, "-virtual") {
			if text, ok := obj.Data["All/yarilo-virtual"]; !ok || !strings.Contains(text, "-Trash") {
				t.Errorf("the definitions map holds %v, want the mailbox's own file", obj.Data)
			}
			definitions = true
		}
		if obj.Kind != "StatefulSet" || !strings.HasSuffix(obj.Metadata.Name, "-backend") {
			continue
		}
		for _, c := range obj.Spec.Template.Spec.Containers {
			for _, m := range c.VolumeMounts {
				if m.Name != "virtual-definitions" {
					continue
				}
				if m.MountPath != "/etc/yarilo/virtual" || !m.ReadOnly {
					t.Errorf("container %q mounts them at %q readOnly=%v, want the path mail_path names, read-only", c.Name, m.MountPath, m.ReadOnly)
				}
				mounted[c.Name] = true
			}
		}
	}
	if !definitions {
		t.Fatal("no definitions map was rendered")
	}
	for _, want := range []string{"yarilo-imap", "yarilo-lmtp", "yarilo-fts", "yarilo-jmap"} {
		if !mounted[want] {
			t.Errorf("container %q reads mailboxes and does not mount the definitions", want)
		}
	}
}
