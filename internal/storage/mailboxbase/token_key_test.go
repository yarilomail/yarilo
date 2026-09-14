package mailboxbase

import (
	"path/filepath"
	"testing"

	fileidx "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Two accounts may hold the same folder name at different roots: a key that
// cannot tell them apart skips a scan that was never proven.
func TestTheTokenKeyKeepsAccountsAndFoldersApart(t *testing.T) {
	root := t.TempDir()
	boxFor := func(user, home string) *Box {
		info := &mailbox.UserInfo{Username: user, Home: home}
		return Open(maildir.New().OpenUser(info), fileidx.New().OpenUser(info))
	}
	u := boxFor("u@x.com", filepath.Join(root, "u"))
	v := boxFor("v@x.com", filepath.Join(root, "v"))
	uElsewhere := boxFor("u@x.com", filepath.Join(root, "shared"))

	tests := []struct {
		name string
		a, b string
		same bool
	}{
		{"same account, same folder", u.tokenKey("INBOX"), u.tokenKey("INBOX"), true},
		{"different accounts", u.tokenKey("INBOX"), v.tokenKey("INBOX"), false},
		{"same account, different storage roots", u.tokenKey("INBOX"), uElsewhere.tokenKey("INBOX"), false},
		{"same account, different folders", u.tokenKey("INBOX"), u.tokenKey("Sent"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if (tc.a == tc.b) != tc.same {
				t.Errorf("keys %q / %q: same = %v, want %v", tc.a, tc.b, tc.a == tc.b, tc.same)
			}
		})
	}

	// The separator is one no field can contain, or the boundary between
	// fields is guessable from their contents.
	odd := boxFor("u@x.com\x00", filepath.Join(root, "u"))
	if odd.tokenKey("INBOX") == u.tokenKey("INBOX") {
		t.Error("a name carrying the separator forged another account's key")
	}
}

// Overflow drops the map rather than expiring by age: the bound must cost a
// scan, never a wrong skip.
func TestCacheOverflowCostsAScanNotCorrectness(t *testing.T) {
	c := &syncTokenCache{maxEntries: 4}
	c.put("a", "t")
	for i := 0; i < 4; i++ {
		c.put(string(rune('b'+i)), "t")
	}
	if _, ok := c.get("a"); ok {
		t.Error("entry survived the overflow drop; the bound is not bounding")
	}
	if len(c.tokens) > c.maxEntries {
		t.Errorf("%d entries held, cap is %d", len(c.tokens), c.maxEntries)
	}
	if tok, ok := c.get(string(rune('b' + 3))); !ok || tok != "t" {
		t.Error("the entry that triggered the drop was not kept")
	}
}
