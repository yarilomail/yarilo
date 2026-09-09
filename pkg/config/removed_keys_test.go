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
