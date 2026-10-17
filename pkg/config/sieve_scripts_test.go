package config

import (
	"strings"
	"testing"
)

// The script limits: 0 lifts one, a negative value is refused by name.
func TestScriptLimitsAreOffKeys(t *testing.T) {
	keys := []struct {
		key   string
		field func(*Config) int
	}{
		{"sieve_max_num_scripts", func(c *Config) int { return c.Sieve.MaxScripts }},
		{"sieve_max_storage", func(c *Config) int { return c.Sieve.MaxStorage }},
	}
	cases := []struct {
		value   string
		want    int
		refused bool
	}{{"0", 0, false}, {"5", 5, false}, {"-1", 0, true}}
	for _, k := range keys {
		for _, c := range cases {
			cfg, err := loadYAML(t, yamlAt("sieve."+k.key, c.value))
			switch {
			case c.refused && (err == nil || !strings.Contains(err.Error(), k.key)):
				t.Errorf("%s %s: err = %v, want a refusal naming it", k.key, c.value, err)
			case !c.refused && err != nil:
				t.Errorf("%s %s: %v", k.key, c.value, err)
			case !c.refused && k.field(cfg) != c.want:
				t.Errorf("%s %s loads as %d", k.key, c.value, k.field(cfg))
			}
		}
	}
}

// jmap_submission_ttl_seconds: 0 keeps no record, a negative value is refused.
func TestSubmissionTTLIsAnOffKey(t *testing.T) {
	for _, c := range []struct {
		value   string
		want    int
		refused bool
	}{{"0", 0, false}, {"60", 60, false}, {"-1", 0, true}} {
		cfg, err := loadYAML(t, yamlAt("protocol.jmap.jmap_submission_ttl_seconds", c.value))
		switch {
		case c.refused && (err == nil || !strings.Contains(err.Error(), "jmap_submission_ttl_seconds")):
			t.Errorf("%s: err = %v, want a refusal naming the key", c.value, err)
		case !c.refused && (err != nil || cfg.Protocol.JMAP.SubmissionTTLSeconds != c.want):
			t.Errorf("%s: %v, loads as %v", c.value, err, cfg)
		}
	}
	if Defaults().Protocol.JMAP.SubmissionTTLSeconds != 86400 {
		t.Errorf("default = %d, want 86400", Defaults().Protocol.JMAP.SubmissionTTLSeconds)
	}
}
