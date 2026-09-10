package integration_test

import (
	"strings"
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mbox"
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
		if _, perr := mbox.MessagePath(box, "INBOX", m); perr != nil {
			unnamed++
		}
	}
	return len(msgs), unnamed
}

// A reconcile between the body and the record must not see a file no record
// tracks: it imports it, and the save that follows takes its row (#1736).
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
	if err := mbox.NameSaved(box, "INBOX", saved, m); err != nil {
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			box, idx, f, reconcile := windowFixture(t)
			saved, m := tc.save(t, box, idx, f)

			if st := reconcile(); st.Imported != 0 {
				t.Errorf("the reconcile imported %d files the session was still writing", st.Imported)
			}
			if err := mbox.RecordSaved(idx, box, f.ID, "INBOX", saved, m); err != nil {
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

// A move is the same window one folder further: the file leaves the source and
// must not appear in the destination before the record that names it (#1736).
func TestAReconcileInsideAMoveWindow(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	box := maildir.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	if err := box.Create("Archive"); err != nil {
		t.Fatal(err)
	}
	idx := indexfile.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	src, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := idx.OpenFolder("Archive", 1)
	if err != nil {
		t.Fatal(err)
	}
	rec := mailbox.Driver(box).(interface {
		ReconcileIndex(mailbox.UserIndex, *mailbox.Folder) (mailbox.SyncStats, error)
	})

	const body = "From: a@b\r\nSubject: move\r\n\r\nbody\r\n"
	saved, vsize, guid, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid}
	if err := mbox.RecordSaved(idx, box, src.ID, "INBOX", saved, m); err != nil {
		t.Fatal(err)
	}

	mover := mailbox.Driver(box).(interface {
		Move(string, string, string, [16]byte) (string, [16]byte, error)
	})
	moved, movedGUID, err := mover.Move("INBOX", "Archive", saved, m.GUID)
	if err != nil {
		t.Fatal(err)
	}

	// The window: the file has left INBOX and the destination has no record of
	// it yet. A session selecting Archive reconciles here.
	st, err := rec.ReconcileIndex(idx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if st.Imported != 0 {
		t.Errorf("the reconcile imported %d files a move was still placing", st.Imported)
	}

	nm := &mailbox.MessageMeta{Size: m.Size, VSize: m.VSize, GUID: movedGUID}
	if err := mbox.RecordSaved(idx, box, dst.ID, "Archive", moved, nm); err != nil {
		t.Fatal(err)
	}
	msgs, err := idx.GetMessages(dst.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("one move left %d records in the destination", len(msgs))
	}
	if _, perr := mbox.MessagePath(box, "Archive", msgs[0]); perr != nil {
		t.Errorf("the moved message cannot be named: %v", perr)
	}
}
