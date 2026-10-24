package guard_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
)

var placeholder = regexp.MustCompile(`\$\{([A-Z0-9_]+)\}`)

// unexpanded lists every string under v that still holds "${", by path.
func unexpanded(v reflect.Value, path string, out *[]string) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			unexpanded(v.Elem(), path, out)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				unexpanded(v.Field(i), path+"."+v.Type().Field(i).Name, out)
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			unexpanded(v.Index(i), fmt.Sprintf("%s[%d]", path, i), out)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			unexpanded(v.MapIndex(k), fmt.Sprintf("%s[%v]", path, k), out)
		}
	case reflect.String:
		if placeholder.MatchString(v.String()) {
			*out = append(*out, path)
		}
	}
}

// Every ${VAR} the chart renders is a Secret given through the environment;
// the loader must expand it, or the field holds the placeholder, which is
// public, as its value.
func TestEveryRenderedPlaceholderIsExpanded(t *testing.T) {
	const sandbox = "../../helm_values/values-sandbox.yaml"
	sets := [][]string{nil, {sandbox}}
	overlays, err := filepath.Glob("../../helm_values/values-sandbox-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range overlays {
		sets = append(sets, []string{sandbox, o})
	}
	seen := 0
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
				seen++
			}
			path := filepath.Join(t.TempDir(), "yarilo.yaml")
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			var left []string
			unexpanded(reflect.ValueOf(cfg), "cfg", &left)
			for _, p := range left {
				t.Errorf("%s keeps its ${…} placeholder after load", p)
			}
		})
	}
	if seen == 0 {
		t.Fatal("no placeholder rendered: the guard looks at nothing")
	}
}
