package maildir

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A row another handle appended while this one's window is open, as a COPY
// from another session does, is found by its uid rather than read as damage.
func TestARowAppendedInsideTheWindowIsFound(t *testing.T) {
	home := t.TempDir()
	a, aidx := openWindowUser(t, home)
	b, bidx := openWindowUser(t, home)
	for _, box := range []*userMailbox{a, b} {
		if err := box.Init(); err != nil {
			t.Fatal(err)
		}
	}
	afolder, err := aidx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	deliverOne(t, a, aidx, afolder)
	first := recordAt(t, aidx, afolder, 0)
	if _, err := a.RecordPath("INBOX", first); err != nil {
		t.Fatal(err)
	}
	a.folderCacheFor("INBOX").markChecked()

	bfolder, err := bidx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	deliverOne(t, b, bidx, bfolder)
	second := recordAt(t, bidx, bfolder, 1)

	rc, err := a.OpenRecord("INBOX", second)
	if err != nil {
		t.Fatalf("uid %d, appended by another handle inside the window: %v", second.UID, err)
	}
	rc.Close()
}

// A file renamed by a STORE between the lookup and the open is opened under
// the name it wears now, not answered as missing.
func TestAFileRenamedBeforeTheOpenIsRead(t *testing.T) {
	home := t.TempDir()
	box, idx := openWindowUser(t, home)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	folder, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	deliverOne(t, box, idx, folder)
	m := recordAt(t, idx, folder, 0)
	name, err := box.RecordPath("INBOX", m)
	if err != nil {
		t.Fatal(err)
	}
	cur := filepath.Join(box.folderPath("INBOX"), "cur")
	renamed := maildirBase(name) + ":2,FS"
	restore := SetTestBeforeOpen(func() {
		if err := os.Rename(filepath.Join(cur, name), filepath.Join(cur, renamed)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Error(err)
		}
	})
	defer restore()
	rc, err := box.OpenRecord("INBOX", m)
	if err != nil {
		t.Fatalf("a file renamed between lookup and open: %v", err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if !strings.Contains(string(body), "From: a@b") {
		t.Errorf("read %q, want the message", body)
	}
}

func recordAt(t *testing.T, idx mailbox.UserIndex, f *mailbox.Folder, i int) *mailbox.MessageMeta {
	t.Helper()
	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) <= i {
		t.Fatalf("%d records, want more than %d", len(msgs), i)
	}
	return msgs[i]
}
