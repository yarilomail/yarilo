package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/redisopt"
)

// Each block that opens a Redis client takes the same sentinel_* keys through
// the loader, the password through ${ENV}; half a configuration is refused at
// start with the key named (#2505).
func TestRedisSentinelKeys(t *testing.T) {
	t.Setenv("SENTINEL_PW", "spw")
	full := "sentinel_addrs: [s1:26379, s2:26379]\n%ssentinel_master: mymaster\n%ssentinel_user: sentinel\n%ssentinel_password: ${SENTINEL_PW}\n"
	tests := []struct {
		block  string
		yaml   func(keys string) string
		read   func(c *Config) redisopt.Sentinel
		prefix string
	}{
		{
			block:  "locks_service",
			yaml:   func(k string) string { return "locks_service:\n  " + k },
			read:   func(c *Config) redisopt.Sentinel { return c.LocksService.Sentinel() },
			prefix: "  ",
		},
		{
			block:  "auth.token",
			yaml:   func(k string) string { return "auth:\n  token:\n    " + k },
			read:   func(c *Config) redisopt.Sentinel { return c.Auth.Token.Sentinel() },
			prefix: "    ",
		},
		{
			block:  "warden_service",
			yaml:   func(k string) string { return "warden_service:\n  " + k },
			read:   func(c *Config) redisopt.Sentinel { return c.WardenService.Sentinel() },
			prefix: "  ",
		},
		{
			block: "dicts.shared.settings",
			yaml: func(k string) string {
				return "dicts:\n  shared:\n    driver: redis\n    settings:\n      addr: r:6379\n      " + k
			},
			read:   func(c *Config) redisopt.Sentinel { return redisopt.SentinelFromSettings(c.Dicts["shared"].Settings) },
			prefix: "      ",
		},
	}
	for _, tc := range tests {
		t.Run(tc.block, func(t *testing.T) {
			p := tc.prefix
			cfg, err := loadYAML(t, tc.yaml(strings.ReplaceAll(full, "%s", p)))
			if err != nil {
				t.Fatalf("full configuration: %v", err)
			}
			got := tc.read(cfg)
			if !slices.Equal(got.Addrs, []string{"s1:26379", "s2:26379"}) || got.Master != "mymaster" || got.User != "sentinel" || got.Password != "spw" {
				t.Errorf("read %+v, want both addresses, mymaster, sentinel and the password from the env", got)
			}

			_, err = loadYAML(t, tc.yaml("sentinel_addrs: [s1:26379]\n"))
			if want := tc.block + ".sentinel_master"; err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("addresses without a master = %v, want an error naming %s", err, want)
			}
			_, err = loadYAML(t, tc.yaml("sentinel_master: mymaster\n"))
			if want := tc.block + ".sentinel_addrs"; err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("a master without addresses = %v, want an error naming %s", err, want)
			}
		})
	}
}
