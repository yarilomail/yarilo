package integration_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mbox"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

type reconciler interface {
	ReconcileIndex(mailbox.UserIndex, *mailbox.Folder) (mailbox.SyncStats, error)
}

// openMaildir gives a folder and the two handles a session holds.
func openMaildir(t *testing.T, user string) (mailbox.UserMailbox, mailbox.UserIndex, *mailbox.Folder, string) {
	t.Helper()
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: user, Home: home, Driver: "maildir"}
	box := maildir.New().OpenUser(info)
	t.Cleanup(func() { _ = box.Close() })
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	idx := file.New().OpenUser(info)
	t.Cleanup(func() { _ = idx.Close() })
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	return box, idx, f, filepath.Join(home, "Maildir")
}

// dropInCur writes a file the way another MDA does: straight into cur/, named
// by nobody the index knows.
func dropInCur(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "cur", name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// namelessRecords is the number the slot reads: records the list cannot name.
func namelessRecords(t *testing.T, box mailbox.UserMailbox, idx mailbox.UserIndex, f *mailbox.Folder) []uint32 {
	t.Helper()
	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	var out []uint32
	for _, m := range msgs {
		if name, err := mbox.MessagePath(box, "INBOX", m); err != nil || name == "" {
			out = append(out, m.UID)
		}
	}
	return out
}

// A folder walked the way imaptest walks one holds no record the list cannot
// name: the row is written before the record it belongs to (#1745).
func TestNoRecordIsOlderThanItsRow(t *testing.T) {
	box, idx, f, root := openMaildir(t, "u1@example.com")
	rec := mailbox.Driver(box).(reconciler)

	for round := 0; round < 4; round++ {
		for i := 0; i < 5; i++ {
			body := fmt.Sprintf("From: a@b\r\nSubject: append %d-%d\r\n\r\nbody\r\n", round, i)
			saved, vsize, guid, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, [16]byte{})
			if err != nil {
				t.Fatal(err)
			}
			m := &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid}
			if err := mbox.RecordSaved(idx, box, f.ID, "INBOX", saved, m); err != nil {
				t.Fatal(err)
			}
		}
		// Files another writer put there, which the reconcile has to import.
		for i := 0; i < 3; i++ {
			body := fmt.Sprintf("From: a@b\r\nSubject: dropped %d-%d\r\n\r\nbody\r\n", round, i)
			dropInCur(t, root, fmt.Sprintf("17000000%02d.M%dP%d.host,S=%d,W=%d:2,", round, i, round, len(body), len(body)), body)
		}
		if _, err := rec.ReconcileIndex(idx, f); err != nil {
			t.Fatalf("round %d reconcile: %v", round, err)
		}
		msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{})
		if err != nil {
			t.Fatal(err)
		}
		// Flags reach the filename, and a third of the folder is expunged:
		// the shapes a run walks between reconciles.
		writer := mailbox.Driver(box).(mailbox.FlagWriter)
		for i, m := range msgs {
			name, perr := mbox.MessagePath(box, "INBOX", m)
			if perr != nil {
				continue
			}
			switch i % 3 {
			case 0:
				if _, err := writer.WriteFlags("INBOX", name, []string{`\Seen`}, nil); err != nil {
					t.Fatalf("write flags: %v", err)
				}
			case 1:
				if err := box.Remove("INBOX", name); err != nil {
					t.Fatalf("remove: %v", err)
				}
				if err := idx.ExpungeMessage(f.ID, m.UID); err != nil {
					t.Fatalf("expunge: %v", err)
				}
			}
		}
		if _, err := rec.ReconcileIndex(idx, f); err != nil {
			t.Fatalf("round %d second reconcile: %v", round, err)
		}
	}

	if bad := namelessRecords(t, box, idx, f); len(bad) != 0 {
		t.Fatalf("%d records the list cannot name after the run: %v", len(bad), bad)
	}
}

// A base another writer lists under its own uid between the scan and the write
// is one message skipped and reported, not a batch lost (#1745).
func TestARefusedRowSkipsOneMessageNotTheBatch(t *testing.T) {
	box, idx, f, root := openMaildir(t, "u2@example.com")
	rec := mailbox.Driver(box).(reconciler)

	// One ordinary save so the list exists before another writer touches it.
	const body = "From: a@b\r\nSubject: first\r\n\r\nbody\r\n"
	saved, vsize, guid, serr := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, [16]byte{})
	if serr != nil {
		t.Fatal(serr)
	}
	if err := mbox.RecordSaved(idx, box, f.ID, "INBOX", saved,
		&mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid}); err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		b := fmt.Sprintf("From: a@b\r\nSubject: dropped %d\r\n\r\nbody\r\n", i)
		n := fmt.Sprintf("1700000%03d.M7P7_%d.host,S=%d,W=%d:2,", i, i, len(b), len(b))
		dropInCur(t, root, n, b)
		names = append(names, n)
	}

	// Another pod names the first file mid-reconcile: its row lands under a
	// uid that is not the one we allocated.
	contested := strings.SplitN(names[0], ":2,", 2)[0]
	disarm := maildir.SetTestBeforeRowWrite(func() {
		listPath := filepath.Join(root, "yarilo-uidlist")
		raw, rerr := os.ReadFile(listPath)
		if rerr != nil {
			t.Fatalf("read list: %v", rerr)
		}
		if werr := os.WriteFile(listPath, append(raw, []byte("900 :"+contested+"\n")...), 0o600); werr != nil {
			t.Fatalf("write list: %v", werr)
		}
	})
	st, err := rec.ReconcileIndex(idx, f)
	disarm()
	if err != nil {
		t.Fatalf("the batch failed on one refused row: %v", err)
	}
	if st.Imported != 2 {
		t.Errorf("the reconcile imported %d messages, want 2: one base was taken, the other two are ours", st.Imported)
	}
	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("the folder holds %d records, want 3 -- the saved one and the two ours: %v", len(msgs), uidsOf(msgs))
	}
	if bad := namelessRecords(t, box, idx, f); len(bad) != 0 {
		t.Fatalf("%d records the list cannot name: %v", len(bad), bad)
	}
}

// A reconcile ending between the rows and the records leaves rows naming no
// record; the next one imports them at the uid the list gives (#1745).
func TestRowsWrittenWithoutRecordsHealOnTheNextSync(t *testing.T) {
	box, idx, f, root := openMaildir(t, "u3@example.com")
	rec := mailbox.Driver(box).(reconciler)

	for i := 0; i < 3; i++ {
		b := fmt.Sprintf("From: a@b\r\nSubject: dropped %d\r\n\r\nbody\r\n", i)
		dropInCur(t, root, fmt.Sprintf("1700000%03d.M1P1_%d.host,S=%d,W=%d:2,", i, i, len(b), len(b)), b)
	}
	stop := maildir.SetTestStopAfterRows()
	if _, err := rec.ReconcileIndex(idx, f); err == nil {
		stop()
		t.Fatal("the seam did not end the reconcile")
	}
	stop()
	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("the interrupted reconcile left %d records; the rows go first", len(msgs))
	}

	if _, err := rec.ReconcileIndex(idx, f); err != nil {
		t.Fatal(err)
	}
	msgs, err = idx.GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("the folder holds %d records after the healing sync, want 3", len(msgs))
	}
	if bad := namelessRecords(t, box, idx, f); len(bad) != 0 {
		t.Fatalf("%d records the list cannot name after the heal: %v", len(bad), bad)
	}
	for _, m := range msgs {
		if m.UID == 0 || m.UID > 3 {
			t.Errorf("record uid %d did not come from the rows the first pass wrote", m.UID)
		}
	}
}
