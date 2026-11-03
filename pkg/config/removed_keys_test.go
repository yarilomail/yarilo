package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A config naming a setting this build removed is refused by name: started
// quietly, it describes a deployment the binary cannot serve (#1756).
func TestARemovedKeyRefusesTheStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yarilo.yaml")
	const cfg = `
director_service:
  listen: ":9102"
  lmtp_listen: ":24"
`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("a config naming director_service.lmtp_listen started")
	}
	if !strings.Contains(err.Error(), "director_service.lmtp_listen") {
		t.Errorf("the refusal is %q and does not name the key", err)
	}
	if !strings.Contains(err.Error(), "1756") {
		t.Errorf("the refusal is %q and does not say what to do instead", err)
	}
}

// A config that names none of them starts.
func TestAConfigWithoutRemovedKeysStarts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yarilo.yaml")
	if err := os.WriteFile(path, []byte("director_service:\n  listen: \":9102\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("an ordinary config was refused: %v", err)
	}
}

// auth's own SASL listener is gone: Postfix goes to yarilo-sasl-login, and a
// config still naming the old key is told where.
func TestTheAuthSASLListenerKeyRefuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yarilo.yaml")
	if err := os.WriteFile(path, []byte("auth_service:\n  listen: \":9100\"\n  sasl_listen: \":12345\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "auth_service.sasl_listen") || !strings.Contains(err.Error(), "yarilo-sasl-login") {
		t.Fatalf("err %v; want a refusal naming the key and pointing at yarilo-sasl-login", err)
	}
}

// The server's cipher preference has no switch in Go's TLS: a config setting it,
// in general.ssl or in a listener's own ssl, under either name, is refused.
func TestTheServerCipherPreferenceKeyRefuses(t *testing.T) {
	for _, tc := range []struct {
		key  string
		yaml string
	}{
		{"general.ssl.ssl_prefer_server_ciphers", "general:\n  ssl:\n    ssl_prefer_server_ciphers: true\n"},
		{"general.ssl.prefer_server_ciphers", "general:\n  ssl:\n    prefer_server_ciphers: false\n"},
		{"services.imaps_login.ssl.ssl_prefer_server_ciphers",
			"services:\n  imaps_login:\n    enabled: true\n    port: 993\n    ssl:\n      ssl_prefer_server_ciphers: true\n"},
	} {
		path := filepath.Join(t.TempDir(), "yarilo.yaml")
		if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		if err == nil || !strings.Contains(err.Error(), tc.key) || !strings.Contains(err.Error(), "Go 1.17") {
			t.Errorf("%s: Load gave %v, want a refusal naming the key and why", tc.key, err)
		}
	}
}
