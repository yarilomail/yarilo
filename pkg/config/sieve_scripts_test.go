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

// The vacation periods: 0 lifts a bound, a negative value is refused by name;
// the defaults are the reference's (#2277).
func TestVacationKeys(t *testing.T) {
	periods := []struct {
		key   string
		field func(*Config) int
		def   int
	}{
		{"sieve_vacation_min_period", func(c *Config) int { return c.Sieve.VacationMinPeriod }, 86400},
		{"sieve_vacation_max_period", func(c *Config) int { return c.Sieve.VacationMaxPeriod }, 5184000},
		{"sieve_vacation_default_period", func(c *Config) int { return c.Sieve.VacationDefaultPeriod }, 604800},
	}
	for _, k := range periods {
		if got := k.field(Defaults()); got != k.def {
			t.Errorf("%s default = %d, want %d", k.key, got, k.def)
		}
		for _, c := range []struct {
			value   string
			want    int
			refused bool
		}{{"0", 0, false}, {"60", 60, false}, {"-1", 0, true}} {
			cfg, err := loadYAML(t, yamlAt("sieve."+k.key, c.value))
			switch {
			case c.refused && (err == nil || !strings.Contains(err.Error(), k.key)):
				t.Errorf("%s %s: err = %v, want a refusal naming it", k.key, c.value, err)
			case !c.refused && (err != nil || k.field(cfg) != c.want):
				t.Errorf("%s %s: %v, loads as %d", k.key, c.value, err, k.field(cfg))
			}
		}
	}
	d := Defaults().Sieve
	if !d.VacationCheckRecipient || d.VacationUseOriginalRecipient || d.VacationSendFromRecipient || d.VacationToHeaderIgnoreEnvelope ||
		d.VacationDefaultSubject != "" || d.VacationDefaultSubjectTemplate != "" {
		t.Errorf("vacation defaults = %+v, want check_recipient on and the rest off and empty", d)
	}
}
