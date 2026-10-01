package maildir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func uidSpaceResets(t *testing.T) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "fileindex_uid_space_reset_total" {
			return mf.GetMetric()[0].GetCounter().GetValue()
		}
	}
	return 0
}

// openInbox opens INBOX the way a session does, settling the store first.
func openInbox(t *testing.T, box *userMailbox, idx mailbox.UserIndex) *mailbox.Folder {
	t.Helper()
	f, err := mailboxbase.Open(box, idx).Folder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// stamp is a file's bytes and modification time; a missing file reads as empty.
func stamp(t *testing.T, path string) string {
	t.Helper()
	fi, err := os.Stat(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime().String() + "\x00" + string(b)
}

// Every open checks the list's UID space against the index's, though the walk
// has nothing to write: a folder of another generation resets once, and stays.
func TestAnOpenAlignsAFolderTheWalkSkips(t *testing.T) {
	box, idx, _ := recSetup(t)
	for _, f := range []string{"1700000001.M1P1.host", "1700000002.M2P2.host"} {
		deliverToCur(t, box, f+":2,", "From: a@b\r\n\r\nx\r\n")
	}
	ageDirs(t, box)
	openInbox(t, box, idx)
	rows := readUIDList(t, box)
	writeList(t, box, "3 V777 N14 G00000000000000000000000000000000",
		"11 :"+strings.SplitN(rows[0], " :", 2)[1], "12 :"+strings.SplitN(rows[1], " :", 2)[1])

	before := uidSpaceResets(t)
	f := openInbox(t, box, idx)
	if f.UIDValidity != 777 {
		t.Errorf("the folder's UIDVALIDITY is %d after the open, want the list's 777", f.UIDValidity)
	}
	if got := folderUIDs(t, idx, f); len(got) != 2 || got[0] != 11 || got[1] != 12 {
		t.Errorf("the folder holds uids %v, want the list's 11 and 12", got)
	}
	openInbox(t, box, idx)
	if got := uidSpaceResets(t) - before; got != 1 {
		t.Errorf("two opens reset the folder %v times, want once", got)
	}
}

// A folder whose list and index agree is neither reset, walked nor written by an
// open.
func TestAnOpenOfAnAlignedFolderWritesNothing(t *testing.T) {
	box, idx, _ := recSetup(t)
	deliverToCur(t, box, "1700000001.M1P1.host:2,", "From: a@b\r\n\r\nx\r\n")
	ageDirs(t, box)
	openInbox(t, box, idx)
	dir := idx.(interface{ IndexDirFor(string) string }).IndexDirFor("INBOX")
	files := []string{box.uidListPath("INBOX"), filepath.Join(dir, "yarilo.index"), filepath.Join(dir, "yarilo.index.log")}
	was := map[string]string{}
	for _, p := range files {
		was[p] = stamp(t, p)
	}
	before := uidSpaceResets(t)
	walks := 0
	defer mailboxbase.SetWalkedFolder(func(string) { walks++ })()
	openInbox(t, box, idx)
	if got := uidSpaceResets(t) - before; got != 0 || walks != 0 {
		t.Errorf("an aligned folder was reset %v times and walked %d, want neither", got, walks)
	}
	for _, p := range files {
		if stamp(t, p) != was[p] {
			t.Errorf("an open of an aligned folder wrote %s", filepath.Base(p))
		}
	}
}

// ageDirs dates cur/ and new/ an hour back, so a walk's check time vouches for
// them and only a moved token or a forgotten one walks again.
func ageDirs(t *testing.T, box *userMailbox) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	for _, sub := range []string{"cur", "new"} {
		if err := os.Chtimes(filepath.Join(box.folderPath("INBOX"), sub), old, old); err != nil {
			t.Fatal(err)
		}
	}
}
