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

// A session links no dict engine, so a configured dict with no service to reach
// it is a config error at startup, not a nil dict at the first lookup (#1733).
func TestBackendRefusesAConfiguredDictWithNoService(t *testing.T) {
	cfg := &config.Config{}
	cfg.Storage.MaildirRoot = t.TempDir()
	cfg.AuthService.Addr = "127.0.0.1:1"
	cfg.Dicts = map[string]config.DictConfig{"metadata": {Driver: "redis"}}

	_, err := New(cfg)
	if !errors.Is(err, ErrNoDictService) {
		t.Fatalf("New with a dict and no dict_addr gave %v, want ErrNoDictService", err)
	}
}
