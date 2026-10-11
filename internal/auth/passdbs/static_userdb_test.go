package passdbs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
	"github.com/yarilomail/yarilo/pkg/config"
)

// A userdb-only lookup (LMTP, quota-status) through a chain ending in a static
// entry: username_filter keeps the static entry to the names it names.
func TestStaticUserdbAnswersOnlyKnownUsers(t *testing.T) {
	passwd := filepath.Join(t.TempDir(), "passwd")
	if err := os.WriteFile(passwd, []byte("alice@x.test:{PLAIN}secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileEntry := config.PassdbEntry{Driver: "passwd-file", PasswdFile: passwd}
	static := func(filter string) config.PassdbEntry {
		return config.PassdbEntry{Driver: "static", StaticPassword: "{PLAIN}s",
			Fields: map[string]string{"userdb_home": "/mail/%d/%n"}, UsernameFilter: filter}
	}
	cases := []struct {
		name   string
		static config.PassdbEntry
		user   string
		found  bool
	}{
		{"file user", static("static@x.test"), "alice@x.test", true},
		{"the filtered static name", static("static@x.test"), "static@x.test", true},
		{"an unknown name, filtered static", static("static@x.test"), "nobody@x.test", false},
		{"an unknown name, unfiltered static", static(""), "nobody@x.test", true},
		{"excluded by !", static("*@x.test !nobody@*"), "nobody@x.test", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, userdbs, err := Build([]config.PassdbEntry{fileEntry, c.static})
			if err != nil {
				t.Fatal(err)
			}
			ui, err := protocol.UserdbChain(userdbs).Lookup(c.user)
			if err != nil {
				t.Fatal(err)
			}
			if got := ui != nil; got != c.found {
				t.Errorf("userdb lookup of %s found = %v, want %v", c.user, got, c.found)
			}
		})
	}
}

// The passdb chain skips an entry whose filter refuses the name.
func TestStaticPassdbHonoursTheUsernameFilter(t *testing.T) {
	passdbs, _, err := Build([]config.PassdbEntry{{Driver: "static", StaticPassword: "{PLAIN}s", UsernameFilter: "static@x.test"}})
	if err != nil {
		t.Fatal(err)
	}
	for user, want := range map[string]protocol.Result{"static@x.test": protocol.ResultOK, "alice@x.test": protocol.ResultNext} {
		req := &protocol.Request{Username: user, Password: "s", Fields: protocol.NewFields()}
		if got, _ := passdbs[0].Authenticate(req); got != want {
			t.Errorf("Authenticate(%s) = %v, want %v", user, got, want)
		}
	}
}
