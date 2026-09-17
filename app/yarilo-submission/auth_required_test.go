package main

import (
	"errors"
	"testing"

	authrelay "github.com/yarilomail/yarilo/internal/auth/client"
)

// A submission process that starts without an auth service would accept
// connections it can authenticate nothing on (#1733).
func TestSubmissionRefusesToStartWithoutAnAuthService(t *testing.T) {
	_, err := dialAuthService("", nil)
	if !errors.Is(err, authrelay.ErrNoAuthService) {
		t.Fatalf("an empty auth_service_addr gave %v, want ErrNoAuthService", err)
	}
}
