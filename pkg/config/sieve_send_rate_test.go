package config

import (
	"strings"
	"testing"
)

// The send limit is refused where its store cannot count, only when it is on;
// the backend role, which start and yarctl config check both run, carries it.
func TestSieveSendRateNeedsACountingStore(t *testing.T) {
	for _, c := range []struct {
		name, driver, dictDriver string
		limit                    int
		err                      string
	}{
		{"off on memory", "memory", "", 0, ""},
		{"off on redis without a dict", "redis", "", 0, ""},
		{"file", "file", "", 500, ""},
		{"redis dict", "redis", "redis", 500, ""},
		{"memory", "memory", "", 500, "per process"},
		{"redis without a dict", "redis", "", 500, "needs dicts.sieve_duplicate"},
		{"redis on a sql dict", "redis", "sql", 500, `on "sql"`},
		{"negative", "file", "", -1, "want 0 (off) or more"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Sieve.SendRateLimit = c.limit
			cfg.Sieve.DuplicateDriver = c.driver
			if c.dictDriver != "" {
				cfg.Dicts = map[string]DictConfig{"sieve_duplicate": {Driver: c.dictDriver}}
			}
			err := cfg.checkSieveSendRate()
			if c.err == "" && err != nil || c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
				t.Fatalf("err %v, want %q", err, c.err)
			}
			if c.err != "" {
				if rerr := cfg.CheckRole("yarilo-backend"); rerr == nil || !strings.Contains(rerr.Error(), c.err) {
					t.Fatalf("the backend role check: %v, want %q", rerr, c.err)
				}
			}
		})
	}
}
