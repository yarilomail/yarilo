package backend

import (
	"reflect"
	"testing"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The userdb's identity_aliases reach the storage identity JMAP reads.
func TestResolveUserInfoKeepsTheIdentityAliases(t *testing.T) {
	want := []string{"a@example.com", "b@example.com"}
	got := ResolveUserInfo(&mailbox.Resolver{Root: t.TempDir(), HomeTemplate: "%d/%n"}, "u@example.com",
		&protocol.UserInfo{IdentityAliases: want})
	if got == nil || !reflect.DeepEqual(got.IdentityAliases, want) {
		t.Errorf("IdentityAliases = %v, want %v", got, want)
	}
}
