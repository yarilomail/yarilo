package guard_test

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// sieveGlobalShape is what a render says about the global scripts: the
// ConfigMap's keys (nil: no ConfigMap), and which containers mount them where.
func sieveGlobalShape(t *testing.T, out []byte) (keys []string, mounts map[string]string, volume bool) {
	t.Helper()
	mounts = map[string]string{}
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
							Name string `yaml:"name"`
						} `yaml:"volumes"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("parse chart output: %v", err)
		}
		if obj.Kind == "ConfigMap" && strings.HasSuffix(obj.Metadata.Name, "-sieve-global") {
			keys = []string{}
			for k := range obj.Data {
				keys = append(keys, k)
			}
		}
		if obj.Kind != "StatefulSet" || !strings.HasSuffix(obj.Metadata.Name, "-backend") {
			continue
		}
		for _, v := range obj.Spec.Template.Spec.Volumes {
			volume = volume || v.Name == "sieve-global"
		}
		for _, c := range obj.Spec.Template.Spec.Containers {
			for _, m := range c.VolumeMounts {
				if m.Name == "sieve-global" {
					if !m.ReadOnly {
						t.Errorf("%s mounts the global scripts writable", c.Name)
					}
					mounts[c.Name] = m.MountPath
				}
			}
		}
	}
	slices.Sort(keys)
	return keys, mounts, volume
}

// The stand's values: the global scripts are a ConfigMap mounted at
// sieve_global where Sieve runs, lmtp and imap, and nowhere else (#2283).
func TestSieveGlobalScriptsReachLMTPAndIMAP(t *testing.T) {
	out, err := exec.Command("helm", "template", "../../helm", "-f", "../../helm_values/values-sandbox.yaml").Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	keys, mounts, volume := sieveGlobalShape(t, out)
	if !slices.Equal(keys, []string{"site-keep.sieve"}) || !volume {
		t.Errorf("ConfigMap keys %v, volume %v; want the sandbox's site-keep.sieve and a volume", keys, volume)
	}
	want := map[string]string{"yarilo-imap": "/etc/yarilo/sieve-global", "yarilo-lmtp": "/etc/yarilo/sieve-global"}
	if len(mounts) != len(want) || mounts["yarilo-imap"] != want["yarilo-imap"] || mounts["yarilo-lmtp"] != want["yarilo-lmtp"] {
		t.Errorf("mounted in %v, want %v", mounts, want)
	}
}

// No global scripts in values render nothing: no ConfigMap, volume or mount.
func TestNoSieveGlobalScriptsRenderNothing(t *testing.T) {
	out, err := renderWith(t, "")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	keys, mounts, volume := sieveGlobalShape(t, out)
	if keys != nil || len(mounts) != 0 || volume {
		t.Errorf("with no scripts: ConfigMap keys %v, mounts %v, volume %v", keys, mounts, volume)
	}
}
