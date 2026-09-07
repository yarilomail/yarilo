package dboxv2

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fileidx "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// orphanFolder is a folder holding one guid-named file no record points at, aged
// as given: what an interrupted save leaves, and what a crash leaves behind it.
func orphanFolder(t *testing.T, body string, age time.Duration) (mailbox.UserMailbox, mailbox.UserIndex, *mailbox.Folder, string, string) {
	t.Helper()
	_, mb, home := newTestUser(t)
	idx := fileidx.New().OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home})
	t.Cleanup(func() { idx.Close() }) //nolint:errcheck
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails")

	temp, _, guid, serr := mb.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, [16]byte{})
	if serr != nil {
		t.Fatal(serr)
	}
	// No record is ever written for it, which is what makes it an orphan.
	name := "u." + guidHex(guid)
	if rerr := os.Rename(filepath.Join(dir, temp), filepath.Join(dir, name)); rerr != nil {
		t.Fatal(rerr)
	}
	when := time.Now().Add(-age)
	if terr := os.Chtimes(filepath.Join(dir, name), when, when); terr != nil {
		t.Fatal(terr)
	}
	return mb, idx, folder, dir, name
}

func migrateSdbox(t *testing.T, mb mailbox.UserMailbox, idx mailbox.UserIndex, f *mailbox.Folder) int {
	t.Helper()
	m, ok := mb.(interface {
		MigrateUIDNames(mailbox.UserIndex, *mailbox.Folder) (int, error)
	})
	if !ok {
		t.Fatal("the sdbox driver cannot migrate the names it wrote")
	}
	n, err := m.MigrateUIDNames(idx, f)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return n
}

// A save still in flight is left where it is, and the folder is not marked: its
// own caller names it within the cycle, and the next pass finds it placed.
func TestASaveInFlightHoldsTheMarkerAndKeepsItsFile(t *testing.T) {
	mb, idx, folder, dir, name := orphanFolder(t, "in flight\n", time.Minute)

	migrateSdbox(t, mb, idx, folder)

	if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
		t.Errorf("the file of a save in flight was taken: %v", err)
	}
	marker, ok := idx.(mailbox.UIDNameMarker)
	if !ok {
		t.Fatal("the index cannot answer for the pass")
	}
	if done, err := marker.UIDNamed(folder.ID); err != nil || done {
		t.Errorf("the folder was marked with a save still in flight (done=%v, err=%v)", done, err)
	}
}

// A body a crash left behind is filed as a message of its own, keeping the guid
// its name carried, and the folder is then marked (#1718).
func TestAnOrphanedBodyIsFiledAsAMessage(t *testing.T) {
	const body = "From: a@b\r\n\r\norphan\r\n"
	mb, idx, folder, dir, name := orphanFolder(t, body, 48*time.Hour)
	wantGUID := strings.TrimPrefix(name, "u.")

	if n := migrateSdbox(t, mb, idx, folder); n != 1 {
		t.Fatalf("the pass placed %d messages, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
		t.Errorf("the orphan is still under its guid name: %v", err)
	}
	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("the folder holds %d records, want the orphan's own", len(msgs))
	}
	if got := guidHex(msgs[0].GUID); got != wantGUID {
		t.Errorf("the record carries guid %s, the file carried %s", got, wantGUID)
	}
	rc, oerr := mailbox.OpenMessage(mb, "INBOX", msgs[0])
	if oerr != nil {
		t.Fatalf("the filed message cannot be read: %v", oerr)
	}
	defer rc.Close() //nolint:errcheck
	marker := idx.(mailbox.UIDNameMarker)
	if done, err := marker.UIDNamed(folder.ID); err != nil || !done {
		t.Errorf("the folder was not marked with nothing left to place (done=%v, err=%v)", done, err)
	}
}

// A body no reader accepts is set aside rather than deleted: it is the only
// copy, and what it is worth is the operator's to judge (#1718).
func TestAnUnreadableOrphanIsSetAside(t *testing.T) {
	mb, idx, folder, dir, name := orphanFolder(t, "unreadable\n", 48*time.Hour)
	if err := os.WriteFile(filepath.Join(dir, name), []byte("not a dbox record"), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, name), when, when); err != nil {
		t.Fatal(err)
	}

	migrateSdbox(t, mb, idx, folder)

	if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
		t.Errorf("the unreadable body is still in the folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".broken", name)); err != nil {
		t.Errorf("the unreadable body was not set aside: %v", err)
	}
	marker := idx.(mailbox.UIDNameMarker)
	if done, err := marker.UIDNamed(folder.ID); err != nil || !done {
		t.Errorf("the folder was not marked with the body out of it (done=%v, err=%v)", done, err)
	}
}
