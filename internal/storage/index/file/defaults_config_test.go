package file

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// The size config.Defaults writes resolves to the one New takes with none.
func TestDefaultsAreTheEmptyPurgeMinSize(t *testing.T) {
	p := filepath.Join(t.TempDir(), "yarilo.yaml")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Storage.MailCachePurgeMinSize, New().cachePurgeMinSize; got != want {
		t.Fatalf("defaults %d, empty gives %d", got, want)
	}
}
