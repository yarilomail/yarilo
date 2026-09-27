package guard_test

import (
	"os/exec"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The stand's own configuration, not a shape invented here: a shared virtual
// namespace is only usable when its definitions are mounted where its
// mail_path names them, in the containers that read mailboxes.
func TestSharedVirtualDefinitionsReachTheContainersThatReadThem(t *testing.T) {
	out, err := exec.Command("helm", "template", "../../helm",
		"-f", "../../helm_values/values-sandbox.yaml").Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}

	mounted := map[string]bool{}
	var placed map[string]string
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
						Volumes []struct {
							Name      string `yaml:"name"`
							ConfigMap struct {
								Items []struct {
									Key  string `yaml:"key"`
									Path string `yaml:"path"`
								} `yaml:"items"`
							} `yaml:"configMap"`
						} `yaml:"volumes"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("parse chart output: %v", err)
		}
		if obj.Kind == "ConfigMap" && strings.HasSuffix(obj.Metadata.Name, "-virtual") {
			if text, ok := obj.Data["All"]; !ok || strings.TrimSpace(text) != "INBOX" {
				t.Errorf("the definitions map holds %v, want one key per mailbox holding its own file", obj.Data)
			}
			definitions = true
		}
		if obj.Kind != "StatefulSet" || !strings.HasSuffix(obj.Metadata.Name, "-backend") {
			continue
		}
		for _, v := range obj.Spec.Template.Spec.Volumes {
			if v.Name != "virtual-definitions" {
				continue
			}
			placed = map[string]string{}
			for _, it := range v.ConfigMap.Items {
				placed[it.Key] = it.Path
			}
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
	// The key cannot hold the path, so the volume must: without this the file
	// lands as /etc/yarilo/virtual/All and the driver finds no mailbox (#2073).
	if placed["All"] != "All/yarilo-virtual" {
		t.Errorf("the volume places the key at %q, want All/yarilo-virtual", placed["All"])
	}
	for _, want := range []string{"yarilo-imap", "yarilo-lmtp", "yarilo-fts", "yarilo-jmap"} {
		if !mounted[want] {
			t.Errorf("container %q reads mailboxes and does not mount the definitions", want)
		}
	}
}
