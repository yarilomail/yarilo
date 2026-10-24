package guard_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	placeholders := 0
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
			for _, m := range placeholder.FindAllStringSubmatch(text, -1) {
				t.Setenv(m[1], "guard-"+m[1])
				placeholders++
			}
			path := filepath.Join(t.TempDir(), "yarilo.yaml")
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("a pod given this configuration refuses to start: %v", err)
			}
			// Every ${VAR} is a Secret through the environment; one the loader
			// does not expand keeps the public placeholder as its value.
			var left []string
			unexpanded(reflect.ValueOf(cfg), "cfg", &left)
			for _, p := range left {
				t.Errorf("%s keeps its ${…} placeholder after load", p)
			}
		})
	}
	if placeholders == 0 {
		t.Fatal("no placeholder rendered: the expansion check looks at nothing")
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
