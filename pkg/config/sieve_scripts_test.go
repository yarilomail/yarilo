package config

import (
	"strings"
	"testing"
)

// sieve_max_num_scripts: 0 lifts the limit, a negative count is refused by name.
func TestScriptCountIsAnOffKey(t *testing.T) {
	cases := []struct {
		value   string
		want    int
		refused bool
	}{{"0", 0, false}, {"5", 5, false}, {"-1", 0, true}}
	for _, c := range cases {
		cfg, err := loadYAML(t, yamlAt("sieve.sieve_max_num_scripts", c.value))
		switch {
		case c.refused && (err == nil || !strings.Contains(err.Error(), "sieve_max_num_scripts")):
			t.Errorf("%s: err = %v, want a refusal naming sieve_max_num_scripts", c.value, err)
		case !c.refused && err != nil:
			t.Errorf("%s: %v", c.value, err)
		case !c.refused && cfg.Sieve.MaxScripts != c.want:
			t.Errorf("%s loads as %d", c.value, cfg.Sieve.MaxScripts)
		}
	}
}
