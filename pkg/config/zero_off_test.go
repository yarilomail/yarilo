package config

import (
	"fmt"
	"strings"
	"testing"
)

// yamlAt writes value at a dotted path.
func yamlAt(path, value string) string {
	var b strings.Builder
	parts := strings.Split(path, ".")
	for i, p := range parts {
		b.WriteString(strings.Repeat("  ", i) + p + ":")
		if i == len(parts)-1 {
			b.WriteString(" " + value)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// An off key: left out it keeps Defaults(), an explicit 0 loads as 0 (off), 7
// as 7, and a negative value is refused by name.
func TestZeroTurnsTheSettingOff(t *testing.T) {
	keys := offKeys(&Config{})
	if len(keys) < 65 {
		t.Fatalf("only %d keys: the table lost rows", len(keys))
	}
	for i, key := range keys {
		t.Run(key.path, func(t *testing.T) {
			field := func(cfg *Config) int { return *offKeys(cfg)[i].v }
			cfg, err := loadYAML(t, "mode: single\n")
			if err != nil {
				t.Fatal(err)
			}
			if got, want := field(cfg), field(Defaults()); got != want {
				t.Errorf("left out, it loads as %d, want the default %d", got, want)
			}
			for _, v := range []int{0, 7} {
				cfg, err := loadYAML(t, yamlAt(key.path, fmt.Sprint(v)))
				if err != nil {
					t.Fatalf("%d: %v", v, err)
				}
				if got := field(cfg); got != v {
					t.Errorf("%d loads as %d", v, got)
				}
			}
			_, err = loadYAML(t, yamlAt(key.path, "-1"))
			if err == nil || !strings.Contains(err.Error(), key.path) {
				t.Errorf("-1 was not refused naming %s: %v", key.path, err)
			}
		})
	}
}

// A required key: left out it keeps its positive default, 7 loads, and an
// explicit 0 or a negative value is refused by name.
func TestARequiredKeyHasNoOff(t *testing.T) {
	keys := requiredKeys(&Config{})
	if len(keys) < 50 {
		t.Fatalf("only %d keys: the table lost rows", len(keys))
	}
	for i, key := range keys {
		t.Run(key.path, func(t *testing.T) {
			field := func(cfg *Config) int { return *requiredKeys(cfg)[i].v }
			cfg, err := loadYAML(t, "mode: single\n")
			if err != nil {
				t.Fatalf("left out: %v", err)
			}
			if got := field(cfg); got <= 0 || got != field(Defaults()) {
				t.Errorf("left out, it loads as %d, want the positive default %d", got, field(Defaults()))
			}
			if cfg, err = loadYAML(t, yamlAt(key.path, "7")); err != nil {
				t.Fatal(err)
			}
			if got := field(cfg); got != 7 {
				t.Errorf("7 loads as %d", got)
			}
			for _, v := range []string{"0", "-1"} {
				if _, err := loadYAML(t, yamlAt(key.path, v)); err == nil || !strings.Contains(err.Error(), key.path) {
					t.Errorf("%s was not refused naming %s: %v", v, key.path, err)
				}
			}
		})
	}
}

// A list entry that leaves a numeric key out takes its default; an explicit 0
// is kept (#2167 review: vhosts 0 is "no traffic").
func TestAListEntryKeepsItsDefaultsWhenItSaysNothing(t *testing.T) {
	cfg, err := loadYAML(t, `
director_service:
  mail_servers:
    - {host: a, port: 143}
    - {host: b, port: 143, vhosts: 0}
quota:
  quota_warnings:
    - {quota_warning_name: w1}
    - {quota_warning_name: w2, quota_warning_percentage: 80}
auth:
  oauth2:
    - {oauth2_mode: local_jwt, oauth2_jwks_url: "https://idp/jwks"}
    - {oauth2_mode: local_jwt, oauth2_jwks_url: "https://idp/jwks", oauth2_http_timeout_ms: 900, oauth2_token_expire_grace_seconds: 0}
`)
	if err != nil {
		t.Fatal(err)
	}
	ms, w, o := cfg.DirectorService.MailServers, cfg.Quota.Warnings, cfg.Auth.OAuth2
	rows := []struct {
		name      string
		got, want int
	}{
		{"mail_servers[0].vhosts", ms[0].Vhosts, 100},
		{"mail_servers[1].vhosts", ms[1].Vhosts, 0},
		{"quota_warnings[0].percentage", w[0].Percentage, 100},
		{"quota_warnings[1].percentage", w[1].Percentage, 80},
		{"oauth2[0].http_timeout_ms", o[0].HTTPTimeoutMs, 5000},
		{"oauth2[0].token_expire_grace_seconds", o[0].TokenExpireGraceSeconds, 60},
		{"oauth2[1].http_timeout_ms", o[1].HTTPTimeoutMs, 900},
		{"oauth2[1].token_expire_grace_seconds", o[1].TokenExpireGraceSeconds, 0},
	}
	for _, r := range rows {
		if r.got != r.want {
			t.Errorf("%s = %d, want %d", r.name, r.got, r.want)
		}
	}
}

// lmtp_user_concurrency_limit keeps its 2.4.1 meaning: -1 is unlimited, 0 is
// refused while LMTP runs.
func TestLMTPConcurrencyKeepsItsReleasedMeaning(t *testing.T) {
	const lmtp = "services:\n  lmtp:\n    enabled: true\n    port: 24\n"
	if _, err := loadYAML(t, lmtp+"protocol:\n  lmtp:\n    lmtp_user_concurrency_limit: -1\n"); err != nil {
		t.Errorf("-1 refused: %v", err)
	}
	if _, err := loadYAML(t, lmtp+"protocol:\n  lmtp:\n    lmtp_user_concurrency_limit: 0\n"); err == nil {
		t.Error("0 was accepted")
	}
}

// A passdb entry's pool settings: unset stays nil (the driver's default), 0
// stays 0, a negative value is refused by name.
func TestPassdbPoolZeroIsOff(t *testing.T) {
	cfg, err := loadYAML(t, "auth:\n  passdb:\n    - driver: static\n      static_password: x\n      max_open_conns: 0\n      conn_max_lifetime: 30\n")
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Auth.Passdb[0]
	if p.MaxOpenConns == nil || *p.MaxOpenConns != 0 || p.ConnMaxLifetime == nil || *p.ConnMaxLifetime != 30 || p.MaxIdleConns != nil {
		t.Errorf("pool = %v %v %v; want 0, 30, unset", p.MaxOpenConns, p.ConnMaxLifetime, p.MaxIdleConns)
	}
	if _, err := loadYAML(t, "auth:\n  passdb:\n    - driver: static\n      static_password: x\n      conn_max_idle_time: -1\n"); err == nil ||
		!strings.Contains(err.Error(), "auth.passdb[0].conn_max_idle_time") {
		t.Errorf("a negative pool setting was not refused by name: %v", err)
	}
}

// A size of nothing is refused where no size has no meaning.
func TestASizeOfNothingIsRefused(t *testing.T) {
	for _, key := range []string{"storage.mdbox_rotate_size", "fts.fts_detection_sample_bytes"} {
		for _, v := range []string{`"0"`, `"0M"`} {
			if _, err := loadYAML(t, yamlAt(key, v)); err == nil || !strings.Contains(err.Error(), key) {
				t.Errorf("%s %s was not refused: %v", key, v, err)
			}
		}
		if _, err := loadYAML(t, yamlAt(key, `"20M"`)); err != nil {
			t.Errorf("%s 20M refused: %v", key, err)
		}
	}
}
