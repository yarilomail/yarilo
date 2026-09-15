package backend

import (
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/config"
)

// A server does not start on a volume whose exclusion it has not proven:
// the alternative is learning it while carrying mail (#1840).
func TestAServerRefusesAnUnprovenVolume(t *testing.T) {
	cfg := &config.Config{}
	cfg.Storage.MaildirRoot = t.TempDir()
	cfg.Storage.LockMethod = "nonsense"

	_, err := New(cfg)
	if err == nil {
		t.Fatal("the server started with a lock method nothing can take")
	}
	if !strings.Contains(err.Error(), "nonsense") {
		t.Errorf("the refusal is %q and does not name the method it could not take", err)
	}
}
