package maildir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A name minted in the last microsecond of a second, with the clock standing
// still, moves into the next second rather than past a million microseconds.
func TestANameTimeAtTheEndOfASecondRollsOver(t *testing.T) {
	nameTime.Lock()
	prevSecs, prevUsecs := nameTime.secs, nameTime.usecs
	future := time.Now().Unix() + 3600
	nameTime.secs, nameTime.usecs = future, 999_999
	nameTime.Unlock()
	t.Cleanup(func() {
		nameTime.Lock()
		nameTime.secs, nameTime.usecs = prevSecs, prevUsecs
		nameTime.Unlock()
	})
	secs, usecs := mintNameTime()
	if secs != future+1 || usecs != 0 {
		t.Errorf("minted %d.%06d, want %d.000000", secs, usecs, future+1)
	}
}

// A move to the folder a message is already in renames nothing: the name and
// the GUID it answers are the ones it had.
func TestAMoveWithinOneFolderKeepsTheName(t *testing.T) {
	box, _ := newBox(t, "u@x.com")
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	body := "From: a@b\r\n\r\nstay\r\n"
	name, _, guid, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	got, gotGUID, err := box.Move("INBOX", "INBOX", name, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if got != name || gotGUID != guid {
		t.Errorf("moved to %q %x, want %q %x", got, gotGUID, name, guid)
	}
}

// A directory left inside cur/ or new/ is not a message: listing and scanning
// the folder report the files only.
func TestADirectoryInsideTheFolderIsNotAMessage(t *testing.T) {
	box, _ := newBox(t, "u@x.com")
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	saveAndRecord(t, box, "INBOX", "From: a@b\r\n\r\nreal\r\n", 1, nil)
	for _, sub := range []string{"cur", "new"} {
		if err := os.Mkdir(filepath.Join(box.folderPath("INBOX"), sub, "1700000000.M1P1.stray"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	msgs, err := box.List("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	// cur/ holds the directory alone: the delivered file is still in new/.
	if len(msgs) != 0 {
		t.Errorf("list reports %d messages from a cur/ holding only a directory", len(msgs))
	}
	if recs, err := box.Scan("INBOX"); err != nil || len(recs) != 1 {
		t.Errorf("scan: %d records, %v; want the one file", len(recs), err)
	}
}

// A list whose header names no next uid still takes a save: the new message
// gets a uid above every row, and the list keeps them all.
func TestAListHeaderWithoutNextUIDStillTakesASave(t *testing.T) {
	box, _ := newBox(t, "u@x.com")
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	saveAndRecord(t, box, "INBOX", "From: a@b\r\n\r\none\r\n", 5, nil)
	rows := readUIDList(t, box)
	writeList(t, box, "3 V1700000000", rows...)
	saveAndRecord(t, box, "INBOX", "From: a@b\r\n\r\ntwo\r\n", 6, nil)
	if uids := listUIDs(t, box); len(uids) != 2 || uids[1] != 6 {
		t.Errorf("the list holds %v, want [5 6]", uids)
	}
}

// A move rolled back puts the body back under its old name: in new/ when the
// name carries no flags part, as an arrival still unread by any client.
func TestARolledBackMoveRestoresAnArrivalToNew(t *testing.T) {
	box, _ := newBox(t, "u@x.com")
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	if err := box.Create("Archive"); err != nil {
		t.Fatal(err)
	}
	orig := saveAndRecord(t, box, "INBOX", "From: a@b\r\n\r\nback\r\n", 1, nil)
	if maildirBase(orig) != orig {
		t.Skipf("the save named %q with a flags part; this row needs an arrival", orig)
	}
	moved, _, err := box.Move("INBOX", "Archive", orig, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if err := box.RestoreMoved("INBOX", orig, "Archive", moved, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(box.folderPath("INBOX"), "new", orig)); err != nil {
		t.Errorf("the body is not back in new/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(box.folderPath("Archive"), "new", moved)); !os.IsNotExist(err) {
		t.Errorf("the moved copy is still in Archive: %v", err)
	}
}

// Writes the disk refuses at create time fail the verb and leave nothing: a
// save into a read-only tmp/, a folder made under a read-only root.
func TestCreatesTheDiskRefusesFailCleanly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through file modes")
	}
	box, _ := newBox(t, "u@x.com")
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(box.folderPath("INBOX"), "tmp")
	if err := os.Chmod(tmp, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tmp, 0o700) })
	body := "From: a@b\r\n\r\nrefused\r\n"
	if _, _, _, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{}); err == nil {
		t.Error("saved into a read-only tmp/")
	}
	if err := box.Create("Work"); err != nil {
		t.Fatal(err)
	}
	root := box.folderPath("INBOX")
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	if err := box.Create("Refused"); err == nil {
		t.Error("made a folder under a read-only root")
	}
	if err := box.Rename("Work", "Elsewhere"); err == nil {
		t.Error("renamed a folder under a read-only root")
	}
	if err := box.Delete("Work"); err == nil {
		t.Error("deleted a folder under a read-only root")
	}
	if _, err := os.Stat(box.folderPath("Work")); err != nil {
		t.Errorf("the folder is gone after refused changes: %v", err)
	}
}

// An account whose home will not take its maildir fails to open, rather than
// opening on a tree that cannot hold mail.
func TestAHomeThatWillNotTakeTheMaildirFailsInit(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through file modes")
	}
	home := t.TempDir()
	if err := os.Chmod(home, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
	for name, info := range map[string]*mailbox.UserInfo{
		"default layout": {Username: "u@x.com", Home: home},
		"mail path set":  {Username: "u@x.com", Home: home, MailPath: filepath.Join(home, "mail")},
	} {
		if err := New().OpenUser(info).Init(); err == nil {
			t.Errorf("%s: init succeeded in a read-only home", name)
		}
	}
}

// Renames the disk refuses fail the verb and leave the message where
// it was: a move into a read-only folder, a flag write or removal in one.
func TestRenamesTheDiskRefusesLeaveTheMessage(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through file modes")
	}
	box, _ := newBox(t, "u@x.com")
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	if err := box.Create("Archive"); err != nil {
		t.Fatal(err)
	}
	name := saveAndRecord(t, box, "INBOX", "From: a@b\r\n\r\nstays\r\n", 1, []string{`\Seen`})
	lock := func(dir string) {
		for _, sub := range []string{"cur", "new", "tmp"} {
			p := filepath.Join(dir, sub)
			if err := os.Chmod(p, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(p, 0o700) })
		}
	}
	lock(box.folderPath("Archive"))
	if _, _, err := box.Move("INBOX", "Archive", name, [16]byte{}); err == nil {
		t.Error("moved into a read-only folder")
	}
	lock(box.folderPath("INBOX"))
	if _, err := box.WriteFlags("INBOX", name, []string{`\Flagged`}, nil); err == nil {
		t.Error("wrote flags in a read-only folder")
	}
	if err := box.Remove("INBOX", name); err == nil {
		t.Error("removed a message from a read-only folder")
	}
	if _, ok := box.locate("INBOX", name); !ok {
		t.Error("the message left INBOX after refused changes")
	}
}
