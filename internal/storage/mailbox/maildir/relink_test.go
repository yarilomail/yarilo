package maildir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A record the list stopped naming gets its row back, counted as its own
// number: importing the file instead left the message there twice (#1785).
func TestReconcile_UnlistedRecordIsRelinkedNotImported(t *testing.T) {
	box, idx, folder := recSetup(t)

	b := mailboxbase.Open(box, idx)
	const body = "From: a@b\r\nSubject: relink\r\n\r\nbody\r\n"
	name, vsize, guid, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	meta := &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid}
	if err := b.RecordSaved(folder, "INBOX", name, meta); err != nil {
		t.Fatalf("record: %v", err)
	}
	// The row alone is removed: the record and its file both stay.
	if err := os.Remove(filepath.Join(box.controlFolderPath("INBOX"), UIDListFileName)); err != nil {
		t.Fatalf("remove uidlist: %v", err)
	}

	st, err := box.ReconcileIndex(b, idx, folder)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if st.Relinked != 1 || st.Imported != 0 {
		t.Fatalf("relinked=%d imported=%d, want 1 and 0", st.Relinked, st.Imported)
	}
	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("the folder holds %d records, want the one", len(msgs))
	}
	settled, nerr := b.MessagePath("INBOX", msgs[0])
	if nerr != nil {
		t.Fatalf("the relinked record cannot be named: %v", nerr)
	}
	got, known := box.UIDFor("INBOX", settled)
	if !known || got != msgs[0].UID {
		t.Errorf("the list names %q as uid %d (known=%v), want uid %d", settled, got, known, msgs[0].UID)
	}
}
