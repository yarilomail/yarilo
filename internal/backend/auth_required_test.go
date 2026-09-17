package backend

import (
	"errors"
	"testing"

	authrelay "github.com/yarilomail/yarilo/internal/auth/client"
	"github.com/yarilomail/yarilo/pkg/config"
)

// The session binaries verify no credential themselves, so a backend with no
// auth service configured must refuse to come up rather than serve (#1733).
func TestBackendRefusesToStartWithoutAnAuthService(t *testing.T) {
	cfg := &config.Config{}
	cfg.Storage.MaildirRoot = t.TempDir()

	_, err := New(cfg)
	if !errors.Is(err, authrelay.ErrNoAuthService) {
		t.Fatalf("New with no auth_service_addr gave %v, want ErrNoAuthService", err)
	}
}
