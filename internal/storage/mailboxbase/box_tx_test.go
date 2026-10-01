package mailboxbase_test

import (
	"fmt"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// txPair is one account opened twice over one directory, as two sessions in
// two processes: the box under test and another session's index.
func txPair(t *testing.T, uids ...uint32) (mailbox.Box, mailbox.UserIndex, *mailbox.Folder, uint64) {
	t.Helper()
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
	for _, uid := range uids {
		if err := mine.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Size: 10}); err != nil {
			t.Fatal(err)
		}
	}
	of, err := other.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	return box, other, f, of.ID
}

func flagsOf(t *testing.T, box mailbox.Box, f *mailbox.Folder) map[uint32][]string {
	t.Helper()
	msgs, err := box.Messages(f.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[uint32][]string{}
	for _, m := range msgs {
		out[m.UID] = m.Flags
	}
	return out
}

// A change decided on a record applies to that record as read: one another
// session moved in between is skipped and named, the rest is written (#1805).
func TestATransactionWritesWhatItReadAndSkipsWhatMoved(t *testing.T) {
	flagged := mailbox.FlagsUpdate{Mode: mailbox.FlagsAdd, Flags: []string{`\Flagged`}}
	for _, tc := range []struct {
		name        string
		meanwhile   func(other mailbox.UserIndex, id uint64) error
		ops         func(tx mailbox.BoxTx)
		wantSkipped []uint32
		wantUIDs    []uint32
		wantFlagged []uint32
	}{
		{
			name:      "flags of uid 1 changed: its change skipped, uid 2's written",
			meanwhile: func(o mailbox.UserIndex, id uint64) error { return o.AddFlags(id, 1, []string{`\Seen`}, nil) },
			ops: func(tx mailbox.BoxTx) {
				tx.UpdateFlags(1, flagged)
				tx.UpdateFlags(2, flagged)
			},
			wantSkipped: []uint32{1},
			wantUIDs:    []uint32{1, 2, 3},
			wantFlagged: []uint32{2},
		},
		{
			name:      "uid 1 expunged: its expunge skipped, uid 3's change written",
			meanwhile: func(o mailbox.UserIndex, id uint64) error { return o.ExpungeMessage(id, 1) },
			ops: func(tx mailbox.BoxTx) {
				tx.Expunge(1)
				tx.UpdateFlags(3, flagged)
			},
			wantSkipped: []uint32{1},
			wantUIDs:    []uint32{2, 3},
			wantFlagged: []uint32{3},
		},
		{
			name: "a delivery arrived: everything written",
			meanwhile: func(o mailbox.UserIndex, id uint64) error {
				return o.AppendMessage(id, &mailbox.MessageMeta{UID: 4, Size: 10})
			},
			ops: func(tx mailbox.BoxTx) {
				tx.Expunge(1)
				tx.UpdateFlags(2, flagged)
			},
			wantUIDs:    []uint32{2, 3, 4},
			wantFlagged: []uint32{2},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box, other, f, otherID := txPair(t, 1, 2, 3)
			tx, err := box.Begin(f.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Messages(mailbox.SeqSet{}); err != nil {
				t.Fatal(err)
			}
			if err := tc.meanwhile(other, otherID); err != nil {
				t.Fatal(err)
			}
			tc.ops(tx)
			res, err := tx.Commit()
			if err != nil {
				t.Fatalf("the transaction failed whole: %v", err)
			}
			if fmt.Sprint(res.Skipped) != fmt.Sprint(tc.wantSkipped) {
				t.Errorf("skipped %v, want %v", res.Skipped, tc.wantSkipped)
			}
			flags := flagsOf(t, box, f)
			var uids []uint32
			for _, uid := range []uint32{1, 2, 3, 4} {
				if _, ok := flags[uid]; ok {
					uids = append(uids, uid)
				}
			}
			if fmt.Sprint(uids) != fmt.Sprint(tc.wantUIDs) {
				t.Errorf("the folder holds %v, want %v", uids, tc.wantUIDs)
			}
			var flagged []uint32
			for _, uid := range uids {
				for _, fl := range flags[uid] {
					if fl == `\Flagged` {
						flagged = append(flagged, uid)
					}
				}
			}
			if fmt.Sprint(flagged) != fmt.Sprint(tc.wantFlagged) {
				t.Errorf("flagged %v, want %v", flagged, tc.wantFlagged)
			}
		})
	}
}

// An append depends on nothing read, so it is written beside a change that
// was skipped.
func TestAnAppendIsWrittenWhateverMoved(t *testing.T) {
	box, other, f, otherID := txPair(t, 1)
	tx, err := box.Begin(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Messages(mailbox.SeqSet{}); err != nil {
		t.Fatal(err)
	}
	if err := other.ExpungeMessage(otherID, 1); err != nil {
		t.Fatal(err)
	}
	tx.Expunge(1)
	tx.Append(&mailbox.MessageMeta{Size: 10})
	res, err := tx.Commit()
	if err != nil || fmt.Sprint(res.Skipped) != "[1]" {
		t.Fatalf("commit answered %+v, %v; want uid 1 skipped and the append written", res, err)
	}
	if got := flagsOf(t, box, f); len(got) != 1 {
		t.Errorf("the folder holds %d records, want the appended one", len(got))
	}
}
