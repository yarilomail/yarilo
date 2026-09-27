package guard_test

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// configMapKey is what the API server accepts as a key of a ConfigMap or a
// Secret: letters, digits, '-', '_' and '.', and nothing else.
var configMapKey = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)

// A key the API server refuses makes the whole release unappliable, and helm
// template alone never says so -- it is valid YAML either way (#2073).
func TestEveryRenderedConfigMapKeyIsOneTheAPIServerAccepts(t *testing.T) {
	// The stand's own values, which define virtualDefinitions: a key built from
	// an operator's name is the only place an invalid one can come from.
	out, err := exec.Command("helm", "template", "../../helm",
		"-f", "../../helm_values/values-sandbox.yaml").Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}

	seen := 0
	for _, doc := range strings.Split(string(out), "\n---\n") {
		var obj struct {
			Kind       string            `yaml:"kind"`
			Data       map[string]string `yaml:"data"`
			StringData map[string]string `yaml:"stringData"`
			Metadata   struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("parse chart output: %v", err)
		}
		if obj.Kind != "ConfigMap" && obj.Kind != "Secret" {
			continue
		}
		seen++
		for _, keys := range []map[string]string{obj.Data, obj.StringData} {
			for k := range keys {
				if !configMapKey.MatchString(k) {
					t.Errorf("%s %q holds key %q, which the API server refuses", obj.Kind, obj.Metadata.Name, k)
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("the chart rendered no ConfigMap or Secret, so nothing was checked")
	}
}
