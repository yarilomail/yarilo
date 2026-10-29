package mailboxbase_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A driver that does not heal is never handed a corrupt mark it cannot clear:
// the mark is refused, and a heal asked of it does nothing and is no error.
func TestADriverThatDoesNotHealIsNotMarked(t *testing.T) {
	info := &mailbox.UserInfo{Username: "u@example.com", Home: t.TempDir(), Driver: "maildir"}
	store := maildir.New().OpenUser(info)
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	idx := file.New().OpenUser(info)
	box := mailboxbase.Open(store, idx)
	defer box.Close()
	f, err := box.Folder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := fmt.Errorf("read: %w", mailbox.ErrCorruptStorage)
	if box.MarkCorruptOnFetchErr("INBOX", corrupt) {
		t.Error("marked a folder its driver cannot heal")
	}
	if box.MarkCorruptOnFetchErr("INBOX", errors.New("not corruption")) {
		t.Error("marked a folder for an error that is not corruption")
	}
	gone, err := box.HealCorrupt(f)
	if err != nil || gone != nil {
		t.Errorf("heal of a non-healing driver: %v, %v", gone, err)
	}
}
