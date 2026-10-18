package main

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/sieve"
	"github.com/yarilomail/yarilo/pkg/config"
)

// JMAP takes the sieve section's limits, the ones ManageSieve enforces.
func TestSieveScriptsTakeTheSieveSection(t *testing.T) {
	cfg := &config.Config{}
	cfg.Sieve.MaxScriptSize, cfg.Sieve.MaxScripts, cfg.Sieve.MaxStorage = 100, 3, 500
	cfg.Sieve.MaxRedirects = 7
	cfg.Sieve.SieveExtensions = []string{"fileinto"}
	st := sieve.NewScriptStore("fs", "", nil, nil)
	got := sieveScripts(cfg, st)
	if got.Store != st || got.Quota != (sieve.Quota{MaxSize: 100, MaxScripts: 3, MaxStorage: 500}) ||
		got.MaxRedirects != 7 || len(got.Extensions) != 1 || got.Extensions[0] != "fileinto" {
		t.Errorf("SieveScripts = %+v, want the sieve section's limits and store", got)
	}
}

// VacationResponse is served only where a script may use vacation.
func TestVacationFollowsTheSieveSection(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		exts    []string
		want    bool
	}{
		{"enabled, every extension", true, nil, true},
		{"enabled, vacation listed", true, []string{"vacation", "fileinto"}, true},
		{"enabled, vacation not listed", true, []string{"fileinto"}, false},
		{"sieve_vacation_enabled off", false, nil, false},
	}
	for _, c := range cases {
		cfg := &config.Config{}
		cfg.Sieve.VacationEnabled, cfg.Sieve.SieveExtensions = c.enabled, c.exts
		if got := sieveScripts(cfg, sieve.NewScriptStore("fs", "", nil, nil)).Vacation; got != c.want {
			t.Errorf("%s: Vacation = %v, want %v", c.name, got, c.want)
		}
	}
}
