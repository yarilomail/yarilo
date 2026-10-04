package config

import "testing"

// A Redis password comes from the environment (a Secret), never from the
// rendered file; the URL stays as written.
func TestRedisPasswordsExpandFromTheEnvironment(t *testing.T) {
	t.Setenv("TEST_REDIS_PW", "p@ss:w/rd")
	cfg, err := loadYAML(t, personalNS+`
auth:
  token:
    backend: redis
    redis_addr: "redis://r:6379/0"
    redis_password: "${TEST_REDIS_PW}"
warden_service:
  state_backend: redis
  redis_addr: "redis://r:6379/0"
  redis_password: "${TEST_REDIS_PW}"
locks_service:
  mode: remote
  redis: "redis://r:6379/0"
  redis_password: "${TEST_REDIS_PW}"
`)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{
		"auth.token":     cfg.Auth.Token.RedisPassword,
		"warden_service": cfg.WardenService.RedisPassword,
		"locks_service":  cfg.LocksService.RedisPassword,
	} {
		if got != "p@ss:w/rd" {
			t.Errorf("%s.redis_password = %q, want the environment's", name, got)
		}
	}
	if cfg.Auth.Token.RedisAddr != "redis://r:6379/0" {
		t.Errorf("redis_addr changed: %q", cfg.Auth.Token.RedisAddr)
	}
}
