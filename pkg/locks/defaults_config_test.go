package locks_test

import (
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/locks"
)

// The prefixes config.Defaults writes are the backend's own, byte for byte:
// a difference would part the lock keys in Valkey across an upgrade.
func TestDefaultsAreTheBackendPrefixes(t *testing.T) {
	d := config.Defaults().LocksService
	key, ch := locks.DefaultPrefixes()
	if d.KeyPrefix != key || d.ChannelPrefix != ch {
		t.Fatalf("defaults %q %q, the backend %q %q", d.KeyPrefix, d.ChannelPrefix, key, ch)
	}
}
