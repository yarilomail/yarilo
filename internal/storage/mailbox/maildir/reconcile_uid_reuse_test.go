package maildir

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// oneDelivered is a reconciled folder holding one message in cur/: its record
// and the name it has on disk.
func oneDelivered(t *testing.T) (*userMailbox, mailbox.UserIndex, *mailbox.Folder, *mailbox.MessageMeta, string) {
	t.Helper()
	box, idx, folder := recSetup(t)
	deliverToNew(t, box, "1700000001.M1Pa.host", "body\r\n")
	if _, err := box.ReconcileIndex(mailboxbase.Open(box, idx), idx, folder); err != nil {
		t.Fatal(err)
	}
	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("index = %v, err = %v", msgs, err)
	}
	return box, idx, folder, msgs[0], storedName(t, box, "INBOX", msgs[0])
}

func reconcileAgain(t *testing.T, box *userMailbox, idx mailbox.UserIndex, folder *mailbox.Folder) mailbox.SyncStats {
	t.Helper()
	f, err := idx.OpenFolder("INBOX", folder.UIDValidity)
	if err != nil {
		t.Fatal(err)
	}
	st, err := box.ReconcileIndex(mailboxbase.Open(box, idx), idx, f)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// A flag change renames the file while the scan runs, so the scan misses it;
// the message is still there and keeps its record (#2176).
func TestAMessageRenamedOutOfTheScansSightKeepsItsRecord(t *testing.T) {
	box, idx, folder, m, name := oneDelivered(t)
	cur := filepath.Join(box.folderPath("INBOX"), "cur")
	aside := filepath.Join(t.TempDir(), name)
	if err := os.Rename(filepath.Join(cur, name), aside); err != nil {
		t.Fatal(err)
	}
	afterScan = func() {
		afterScan = nil
		if err := os.Rename(aside, filepath.Join(cur, renameWithFlags(name, "S"))); err != nil {
			t.Errorf("rename in the window: %v", err)
		}
	}
	defer func() { afterScan = nil }()

	st := reconcileAgain(t, box, idx, folder)

	if st.Expunged != 0 {
		t.Errorf("the pass expunged %d records of a message on disk", st.Expunged)
	}
	after, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil || len(after) != 1 || after[0].UID != m.UID {
		t.Errorf("index = %v, err = %v; want uid %d kept", after, err, m.UID)
	}
}

// A message that vanished takes its row with it: back on disk, it is a new
// message with a new uid, never the tombstoned one (#2176).
func TestAReturningFileNeverGetsItsTombstonedUID(t *testing.T) {
	box, idx, folder, m, name := oneDelivered(t)
	cur := filepath.Join(box.folderPath("INBOX"), "cur")
	aside := filepath.Join(t.TempDir(), name)
	if err := os.Rename(filepath.Join(cur, name), aside); err != nil {
		t.Fatal(err)
	}
	if st := reconcileAgain(t, box, idx, folder); st.Expunged != 1 {
		t.Fatalf("the pass expunged %d records of a vanished message, want 1", st.Expunged)
	}
	if uid, known := box.UIDFor("INBOX", name); known {
		t.Errorf("the list still names %s as uid %d after its record was expunged", name, uid)
	}

	if err := os.Rename(aside, filepath.Join(cur, name)); err != nil {
		t.Fatal(err)
	}
	reconcileAgain(t, box, idx, folder)

	after, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil || len(after) != 1 {
		t.Fatalf("index = %v, err = %v", after, err)
	}
	if after[0].UID == m.UID {
		t.Errorf("the returning file took uid %d back from its tombstone", m.UID)
	}
}
