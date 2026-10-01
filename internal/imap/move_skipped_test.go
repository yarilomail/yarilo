package imap

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A MOVE source record another session changed after the read stays, unless
// its body already moved: then nothing would be left for it to name (#1805).
func TestMoveKeepsAChangedCopyAndDropsAChangedMove(t *testing.T) {
	for _, tc := range []struct {
		name     string
		moved    bool
		changed  bool
		wantKept bool
	}{
		{"copied, changed meanwhile: kept", false, true, true},
		{"moved in place, changed meanwhile: expunged", true, true, false},
		{"copied, untouched: expunged", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			info := func() *mailbox.UserInfo {
				return &mailbox.UserInfo{Username: "u@example.com", Home: dir, SessionID: "s", Driver: "maildir"}
			}
			store := maildir.New().OpenUser(info())
			mine := file.New().OpenUser(info())
			other := file.New().OpenUser(info())
			t.Cleanup(func() { _ = store.Close(); _ = mine.Close(); _ = other.Close() })
			if err := store.Init(); err != nil {
				t.Fatal(err)
			}
			box := mailboxbase.Open(store, mine, mailboxbase.ReadOnly())
			f, err := box.Folder("INBOX", 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := mine.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Size: 10}); err != nil {
				t.Fatal(err)
			}
			s := &session{userInfo: info(), folder: f}
			tx, err := box.Begin(f.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Messages(mailbox.SeqSet{}); err != nil {
				t.Fatal(err)
			}
			if tc.changed {
				of, _ := other.OpenFolder("INBOX", 0)
				if err := other.AddFlags(of.ID, 1, []string{`\Seen`}, nil); err != nil {
					t.Fatal(err)
				}
			}
			kept := s.expungeSource(box, tx, []uint32{1}, map[uint32]bool{1: tc.moved})
			if kept[1] != tc.wantKept {
				t.Errorf("kept %v, want %v", kept[1], tc.wantKept)
			}
			if there := box.RecordExists(f.ID, 1); there != tc.wantKept {
				t.Errorf("the source record is there %v, want %v", there, tc.wantKept)
			}
		})
	}
}
