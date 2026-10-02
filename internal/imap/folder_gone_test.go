package imap_test

import (
	"strings"
	"testing"

	imap "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A selected mailbox another process deleted is answered NO [NONEXISTENT]
// naming it, not a bare NO.
func TestASelectionDeletedElsewhereIsNonexistent(t *testing.T) {
	root := t.TempDir()
	c := startServerWithRoot(t, maildirBackend(t), root)
	if err := c.Create("Gone", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select("Gone", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	info := (&mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}).UserInfo("user@test.com", "")
	store, idx := maildir.New().OpenUser(info), file.New().OpenUser(info)
	defer store.Close() //nolint:errcheck
	defer idx.Close()   //nolint:errcheck
	if err := mailboxbase.Open(store, idx).Delete("Gone"); err != nil {
		t.Fatal(err)
	}

	_, err := c.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{Flags: true}).Collect()
	if err == nil || !strings.Contains(err.Error(), "NONEXISTENT") || !strings.Contains(err.Error(), "Gone") {
		t.Errorf("FETCH on a mailbox deleted elsewhere: %v; want NO [NONEXISTENT] naming Gone", err)
	}
}
