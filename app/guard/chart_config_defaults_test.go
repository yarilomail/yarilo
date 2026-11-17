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

// The keys the default chart render may set to other than config.Defaults,
// each list with its reason; everything else the chart writes is the binary's
// own default, so yarctl config on an install shows the operator's choices.
var chartDifferences = []struct {
	reason string
	keys   []string
}{
	{"who wrote the file", []string{"chart_version", "config_schema_version"}},
	{"deployment topology: the chart's services, addresses and listeners", []string{
		"mode", "services.", "auth_service.", "backend_api.", "backend_register.", "dict_service.",
		"director_service.listen", "director_service.api.listen", "director_service.ring_tls_server_name",
		"internal_tls.", "locks_client.", "locks_service.", "warden_service.",
	}},
	{"the chart's choice over the binary's", []string{
		"auth.cache.auth_cache_size", "director_service.director_domain_rebalance_percent",
		"sieve.imapsieve_script_dir", "dicts.metadata.", "auth.passdb[0].",
	}},
}

func TestChartRenderDiffersFromDefaultsOnlyWhereListed(t *testing.T) {
	out, err := exec.CommandContext(t.Context(), "helm", "template", "t", "../../helm", "--show-only", "templates/configmap.yaml").Output()
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	var cm struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(out, &cm); err != nil || cm.Data["yarilo.yaml"] == "" {
		t.Fatalf("no yarilo.yaml in the ConfigMap: %v", err)
	}
	p := filepath.Join(t.TempDir(), "yarilo.yaml")
	if err := os.WriteFile(p, []byte(cm.Data["yarilo.yaml"]), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("the default render does not load: %v", err)
	}
	canonical := func(in []config.Setting) []config.Setting {
		var out []config.Setting
		for _, s := range in {
			if !s.Alias {
				out = append(out, s)
			}
		}
		return out
	}
	used := map[string]bool{}
	for _, s := range config.Differing(canonical(config.Walk(cfg, false)), canonical(config.Walk(config.Defaults(), false))) {
		listed := false
		for _, l := range chartDifferences {
			for _, k := range l.keys {
				if s.Key == k || strings.HasSuffix(k, ".") && strings.HasPrefix(s.Key, k) {
					listed, used[k] = true, true
				}
			}
		}
		if !listed {
			t.Errorf("%s = %v: the chart writes other than config.Defaults; put the binary's value in Defaults, or list the key with its reason", s.Key, s.Value)
		}
	}
	for _, l := range chartDifferences {
		for _, k := range l.keys {
			if !used[k] {
				t.Errorf("%s is listed (%s) and the render does not differ there: drop it from the list", k, l.reason)
			}
		}
	}
}
