package mailboxbase_test

import (
	"errors"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The re-read sees another backend's expunge from disk, and keeps a record
// nobody touched (#1690).
func TestRecordExistsSeesAnotherHandlesExpunge(t *testing.T) {
	dir := t.TempDir()
	info := func() *mailbox.UserInfo {
		return &mailbox.UserInfo{Username: "u@example.com", Home: dir, SessionID: "s", Driver: "maildir"}
	}
	// Two index instances over one directory, as two sessions in two processes.
	store := maildir.New().OpenUser(info())
	a := file.New().OpenUser(info())
	b := file.New().OpenUser(info())
	t.Cleanup(func() { _ = store.Close(); _ = a.Close(); _ = b.Close() })
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	box := mailboxbase.Open(store, a, mailboxbase.ReadOnly())
	fa, err := box.Folder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, uid := range []uint32{1, 2} {
		if err := a.AppendMessage(fa.ID, &mailbox.MessageMeta{UID: uid, Size: 10}); err != nil {
			t.Fatal(err)
		}
	}
	fb, err := b.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ExpungeMessage(fb.ID, 1); err != nil {
		t.Fatal(err)
	}
	if box.RecordExists(fa.ID, 1) {
		t.Error("the re-read still sees a record another handle expunged")
	}
	if !box.RecordExists(fa.ID, 2) {
		t.Error("the re-read lost a record nobody touched")
	}
}

type unreadableIndex struct{ mailbox.UserIndex }

func (unreadableIndex) GetMessages(uint64, mailbox.SeqSet) ([]*mailbox.MessageMeta, error) {
	return nil, errors.New("index unavailable")
}

// An index that cannot answer says the record is there: a read fault must not
// be taken for an expunge because the check itself failed.
func TestRecordExistsOnAnUnreadableIndexSaysYes(t *testing.T) {
	box := mailboxbase.Open(maildir.New().OpenUser(&mailbox.UserInfo{Username: "u", Home: t.TempDir()}), unreadableIndex{})
	if !box.RecordExists(1, 7) {
		t.Error("an unreadable index answered that uid 7 is gone")
	}
}
