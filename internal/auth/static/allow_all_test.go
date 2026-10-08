package static

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
)

// A userdb-only lookup asks the passdbs first unless allow_all_users is set,
// as the reference's static userdb does (userdb_static_allow_all_users).
func TestUserdbAsksThePassdbsUnlessAllowAll(t *testing.T) {
	known := func(u string) (bool, error) { return u == "alice@x.test", nil }
	cases := []struct {
		allowAll bool
		user     string
		found    bool
	}{
		{false, "alice@x.test", true},
		{false, "nobody@x.test", false},
		{true, "nobody@x.test", true},
	}
	for _, c := range cases {
		db, err := New(Config{Password: "{PLAIN}s", AllowAllUsers: c.allowAll,
			Fields: map[string]string{"userdb_home": "/mail/%d/%n"}})
		if err != nil {
			t.Fatal(err)
		}
		db.SetUserExists(known)
		ui, err := db.Lookup(c.user)
		if err != nil {
			t.Fatal(err)
		}
		if got := ui != nil; got != c.found {
			t.Errorf("allow_all_users=%v, %s: found = %v, want %v", c.allowAll, c.user, got, c.found)
		}
	}
	var _ protocol.CredentialsLookup = (*DB)(nil)
}
