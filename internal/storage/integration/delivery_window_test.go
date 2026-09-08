package integration_test

import (
	"strings"
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// windowFixture is a maildir folder with a reconciler in hand.
func windowFixture(t *testing.T) (mailbox.UserMailbox, mailbox.UserIndex, *mailbox.Folder, func() mailbox.SyncStats) {
	t.Helper()
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	box := maildir.New().OpenUser(info)
	t.Cleanup(func() { box.Close() }) //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	idx := indexfile.New().OpenUser(info)
	t.Cleanup(func() { idx.Close() }) //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	rec := mailbox.Driver(box).(interface {
		ReconcileIndex(mailbox.UserIndex, *mailbox.Folder) (mailbox.SyncStats, error)
	})
	return box, idx, f, func() mailbox.SyncStats {
		st, rerr := rec.ReconcileIndex(idx, f)
		if rerr != nil {
			t.Fatalf("reconcile: %v", rerr)
		}
		return st
	}
}

// unnamedRecords counts the records that cannot name their file.
func unnamedRecords(t *testing.T, box mailbox.UserMailbox, idx mailbox.UserIndex, f *mailbox.Folder) (records, unnamed int) {
	t.Helper()
	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if _, perr := mailbox.MessagePath(box, "INBOX", m); perr != nil {
			unnamed++
		}
	}
	return len(msgs), unnamed
}

// A save is a body, a name and a record. A reconcile that runs between the
// first two must not see a file no record tracks: it would import it, and the
// delivery that follows would take its row, leaving a record naming nothing.
//
// The reference takes the uidlist lock before moving out of tmp/ for exactly
// this reason, and our save does the same since #1736.
func TestAReconcileInsideADeliveryWindow(t *testing.T) {
	box, idx, f, reconcile := windowFixture(t)

	const body = "From: a@b\r\nSubject: window\r\n\r\nbody\r\n"
	uid, err := idx.AllocateUID(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	saved, vsize, guid, err := box.Save("INBOX", strings.NewReader(body), uid, int64(len(body)), nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}

	// The window: the body is saved, the name is not recorded, the record does
	// not exist. A session selecting the folder reconciles here.
	if st := reconcile(); st.Imported != 0 {
		t.Errorf("the reconcile imported %d files a delivery was still writing", st.Imported)
	}

	m := &mailbox.MessageMeta{UID: uid, Size: uint32(len(body)), VSize: vsize, GUID: guid}
	if err := mailbox.NameSaved(box, "INBOX", saved, m); err != nil {
		t.Fatal(err)
	}
	if err := idx.AppendMessage(f.ID, m); err != nil {
		t.Fatal(err)
	}

	records, unnamed := unnamedRecords(t, box, idx, f)
	if records != 1 {
		t.Errorf("one delivery left %d records", records)
	}
	if unnamed != 0 {
		t.Errorf("%d records cannot name their file", unnamed)
	}
}

// The same window for the paths a session walks: an append, a copy and a move
// all put the body down first and name it after (#1736).
func TestAReconcileInsideASessionWindow(t *testing.T) {
	for _, tc := range []struct {
		name string
		save func(t *testing.T, box mailbox.UserMailbox, idx mailbox.UserIndex, f *mailbox.Folder) (string, *mailbox.MessageMeta)
	}{
		{
			name: "append",
			save: func(t *testing.T, box mailbox.UserMailbox, idx mailbox.UserIndex, f *mailbox.Folder) (string, *mailbox.MessageMeta) {
				const body = "From: a@b\r\nSubject: append\r\n\r\nbody\r\n"
				saved, vsize, guid, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, [16]byte{})
				if err != nil {
					t.Fatal(err)
				}
				return saved, &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid}
			},
		},
		{
			name: "copy",
			save: func(t *testing.T, box mailbox.UserMailbox, idx mailbox.UserIndex, f *mailbox.Folder) (string, *mailbox.MessageMeta) {
				const body = "From: a@b\r\nSubject: copy\r\n\r\nbody\r\n"
				// A copy is a save of the bytes the source handed over.
				saved, vsize, guid, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)),
					[]string{`\Seen`}, [16]byte{})
				if err != nil {
					t.Fatal(err)
				}
				return saved, &mailbox.MessageMeta{Flags: []string{`\Seen`}, Size: uint32(len(body)), VSize: vsize, GUID: guid}
			},
		},
		{
			name: "move",
			save: func(t *testing.T, box mailbox.UserMailbox, idx mailbox.UserIndex, f *mailbox.Folder) (string, *mailbox.MessageMeta) {
				const body = "From: a@b\r\nSubject: move\r\n\r\nbody\r\n"
				saved, vsize, guid, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, [16]byte{})
				if err != nil {
					t.Fatal(err)
				}
				return saved, &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box, idx, f, reconcile := windowFixture(t)
			saved, m := tc.save(t, box, idx, f)

			if st := reconcile(); st.Imported != 0 {
				t.Errorf("the reconcile imported %d files the session was still writing", st.Imported)
			}
			if err := mailbox.RecordSaved(idx, box, f.ID, "INBOX", saved, m); err != nil {
				t.Fatal(err)
			}
			records, unnamed := unnamedRecords(t, box, idx, f)
			if records != 1 {
				t.Errorf("one message left %d records", records)
			}
			if unnamed != 0 {
				t.Errorf("%d records cannot name their file", unnamed)
			}
		})
	}
}
