package backend

import (
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// Only a process that runs Sieve builds the engine and reads the global
// scripts: LMTP delivers through it and IMAP runs IMAPSIEVE. POP3 and
// ManageSieve never run a script, so they read nothing the chart mounts only
// where scripts run, and log no missing file.
func TestOnlyAProcessThatRunsSieveLoadsTheEngine(t *testing.T) {
	on := &config.ServiceConfig{Enabled: true}
	for _, tc := range []struct {
		name string
		set  func(*config.Config)
		want bool
	}{
		{"lmtp", func(c *config.Config) { c.Services.LMTP = on }, true},
		{"imap", func(c *config.Config) { c.Services.IMAP = on }, true},
		{"pop3", func(c *config.Config) { c.Services.POP3 = on }, false},
		{"managesieve", func(c *config.Config) { c.Services.ManageSieve = on }, false},
		{"pop3 and managesieve", func(c *config.Config) { c.Services.POP3, c.Services.ManageSieve = on, on }, false},
		{"all in one", func(c *config.Config) {
			c.Services.IMAP, c.Services.LMTP, c.Services.POP3, c.Services.ManageSieve = on, on, on, on
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Sieve.Enabled = true
			tc.set(cfg)
			if got := sieveRuns(cfg); got != tc.want {
				t.Errorf("sieveRuns = %v, want %v", got, tc.want)
			}
			cfg.Sieve.Enabled = false
			if sieveRuns(cfg) {
				t.Error("sieve off still builds the engine")
			}
		})
	}
}
