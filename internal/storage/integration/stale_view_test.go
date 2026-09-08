package integration_test

import (
	"os"
	"strings"
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A sync whose snapshot predates another writer's append must not treat that
// message as a file nobody holds: the list already names it (#1739).
func TestASyncDoesNotTakeTheRowOfARecordItHasNotSeen(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	dial := embeddedLocks(t)
	locker := dial()

	writer := maildir.New(maildir.WithLocker(locker)).OpenUser(info)
	defer writer.Close() //nolint:errcheck
	if err := writer.Init(); err != nil {
		t.Fatal(err)
	}
	writerIdx := indexfile.New(indexfile.WithLocker(locker)).OpenUser(info)
	defer writerIdx.Close() //nolint:errcheck
	wf, err := writerIdx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}

	// The other session, opened before the append: its view of the folder is
	// what a second pod holds while the first is still writing.
	syncBox := maildir.New(maildir.WithLocker(locker)).OpenUser(info)
	defer syncBox.Close() //nolint:errcheck
	syncIdx := indexfile.New(indexfile.WithLocker(locker)).OpenUser(info)
	defer syncIdx.Close() //nolint:errcheck
	sf, err := syncIdx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := syncIdx.GetMessages(sf.ID, mailbox.SeqSet{}); err != nil {
		t.Fatal(err)
	}

	const body = "From: a@b\r\nSubject: stale view\r\n\r\nbody\r\n"
	saved, vsize, guid, err := writer.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid}
	if err := mailbox.RecordSaved(writerIdx, writer, wf.ID, "INBOX", saved, m); err != nil {
		t.Fatal(err)
	}
	written := m.UID

	// The sync decides on the snapshot it has, as a process does when its
	// reload finds nothing changed; the refresh has to see through it.
	thaw := indexfile.SetTestFreezeReload()
	rec := mailbox.Driver(syncBox).(interface {
		ReconcileIndex(mailbox.UserIndex, *mailbox.Folder) (mailbox.SyncStats, error)
	})
	st, err := rec.ReconcileIndex(syncIdx, sf)
	thaw()
	if err != nil {
		t.Fatal(err)
	}
	if st.Imported != 0 {
		t.Errorf("the sync imported %d files the list already names", st.Imported)
	}

	msgs, err := writerIdx.GetMessages(wf.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("one message left %d records: %v", len(msgs), uidsOf(msgs))
	}
	if msgs[0].UID != written {
		t.Errorf("the record is uid %d, the save wrote %d", msgs[0].UID, written)
	}
	name, perr := mailbox.MessagePath(writer, "INBOX", msgs[0])
	if perr != nil {
		t.Fatalf("uid %d lost its row: %v", msgs[0].UID, perr)
	}
	if _, serr := os.Stat(home + "/Maildir/cur/" + name); serr != nil {
		t.Errorf("the row names %q, which is not there: %v", name, serr)
	}
}

func uidsOf(msgs []*mailbox.MessageMeta) []uint32 {
	out := make([]uint32, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.UID)
	}
	return out
}

// A file the list names keeps its uid when the index no longer holds it: a
// fresh one invalidates a client's cache for a message that never moved (#1593).
func TestAnImportedFileKeepsTheUIDTheListGivesIt(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	box := maildir.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	idx := indexfile.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}

	const body = "From: a@b\r\nSubject: keeps its uid\r\n\r\nbody\r\n"
	saved, vsize, guid, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid}
	if err := mailbox.RecordSaved(idx, box, f.ID, "INBOX", saved, m); err != nil {
		t.Fatal(err)
	}
	written := m.UID

	// The index loses the record; the file and its row stay, which is what an
	// index rebuilt from storage starts from.
	if err := idx.ExpungeMessage(f.ID, written); err != nil {
		t.Fatal(err)
	}

	rec := mailbox.Driver(box).(interface {
		ReconcileIndex(mailbox.UserIndex, *mailbox.Folder) (mailbox.SyncStats, error)
	})
	if _, err := rec.ReconcileIndex(idx, f); err != nil {
		t.Fatal(err)
	}
	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("the folder holds %d records after the import, want one: %v", len(msgs), uidsOf(msgs))
	}
	if msgs[0].UID != written {
		t.Errorf("the imported file came back as uid %d, the list names it %d", msgs[0].UID, written)
	}
	if _, perr := mailbox.MessagePath(box, "INBOX", msgs[0]); perr != nil {
		t.Errorf("the imported record cannot name its file: %v", perr)
	}
}
