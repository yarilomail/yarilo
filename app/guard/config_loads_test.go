package guard_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/yarilomail/yarilo/pkg/config"
)

// helm template only renders, so each kept values file is also loaded the way
// a pod loads it: a refused one took the whole stand down (#2038).
func TestEveryKeptValuesFileLoads(t *testing.T) {
	const sandbox = "../../helm_values/values-sandbox.yaml"
	// Each entry is the -f list a deployment is rendered with: an overlay is
	// applied on top of the sandbox values, the way the stand arm applies it.
	sets := [][]string{nil, {sandbox}}
	overlays, err := filepath.Glob("../../helm_values/values-sandbox-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range overlays {
		sets = append(sets, []string{sandbox, o})
	}
	examples, err := filepath.Glob("../../helm_values/examples/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range examples {
		sets = append(sets, []string{e})
	}
	if len(sets) < 4 {
		t.Fatalf("found only %v: the row would pass by reading nothing", sets)
	}
	for _, set := range sets {
		name := "chart defaults"
		if len(set) > 0 {
			name = filepath.Base(set[len(set)-1])
		}
		t.Run(name, func(t *testing.T) {
			args := []string{"template", "../../helm"}
			for _, f := range set {
				args = append(args, "-f", f)
			}
			out, err := exec.Command("helm", args...).Output()
			if err != nil {
				t.Fatalf("helm template: %v", err)
			}
			text := renderedConfig(t, string(out))
			path := filepath.Join(t.TempDir(), "yarilo.yaml")
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := config.Load(path); err != nil {
				t.Errorf("a pod given this configuration refuses to start: %v", err)
			}
		})
	}
}

// renderedConfig is the yarilo.yaml the chart's ConfigMap carries.
func renderedConfig(t *testing.T, manifests string) string {
	t.Helper()
	for _, doc := range strings.Split(manifests, "\n---\n") {
		var obj struct {
			Kind string            `yaml:"kind"`
			Data map[string]string `yaml:"data"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil || obj.Kind != "ConfigMap" {
			continue
		}
		if text, ok := obj.Data["yarilo.yaml"]; ok {
			return text
		}
	}
	t.Fatal("the chart rendered no ConfigMap carrying yarilo.yaml")
	return ""
}
