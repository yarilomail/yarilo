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
