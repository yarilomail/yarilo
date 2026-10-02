package imap_test

import (
	"strings"
	"testing"

	imap "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/internal/userstate/folders"
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

// RENAME carries the folder's identity record to the new name and keeps
// nothing under the old one.
func TestRenameCarriesTheIdentityRecord(t *testing.T) {
	root := t.TempDir()
	c := startServerWithRoot(t, maildirBackend(t), root)
	if err := c.Create("Work", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.Rename("Work", "Play", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	info := (&mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}).UserInfo("user@test.com", "")
	ids := folders.New(mailbox.ControlRoot(info), info.Username, "", nil)
	if _, known, _ := ids.UIDValidity("Play"); !known {
		t.Error("no identity record under the new name")
	}
	if v, known, _ := ids.UIDValidity("Work"); known {
		t.Errorf("identity record stayed under the old name with uidvalidity %d", v)
	}
}
