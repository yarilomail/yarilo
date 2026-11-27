package authtoken_test

import (
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/authtoken"
	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// The prefix config.Defaults writes is the store's own, byte for byte: a
// difference would orphan every token in Valkey across an upgrade.
func TestDefaultsAreTheStorePrefix(t *testing.T) {
	if got, want := config.Defaults().Auth.Token.KeyPrefix, authtoken.DefaultKeyPrefix(); got != want {
		t.Fatalf("defaults %q, the store %q", got, want)
	}
}
