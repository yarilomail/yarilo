package maildir

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// uidListChildHome, set, makes this test the second writer, in a child process.
const uidListChildHome = "YARILO_UIDLIST_CHILD_HOME"

const rowsPerWriter = 300

// writeRowsAsReconcile writes rows the way a reconcile does: the list held
// for the write.
func writeRowsAsReconcile(u *userMailbox, folder string, entries []listEntry) error {
	release, err := u.holdList(folder, lockSiteReconcileApply)
	if err != nil {
		return err
	}
	defer release()
	_, err = u.recordUIDsHeld(folder, entries, 0)
	return err
}

func writeListRows(t *testing.T, home string, first uint32) {
	t.Helper()
	box := New().OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"})
	defer box.Close() //nolint:errcheck
	u := box.(*userMailbox)
	for uid := first; uid < first+rowsPerWriter; uid++ {
		name := fmt.Sprintf("1700000000.M%dP1.host,S=4,W=4:2,", uid)
		if err := writeRowsAsReconcile(u, "INBOX", []listEntry{{uid: uid, filename: name}}); err != nil {
			t.Fatalf("row %d: %v", uid, err)
		}
	}
}

// Two processes write one folder's list at once, as the FTS service and the
// IMAP backend do. Inside one process a mutex per path hides a lock that does
// not exclude, so the second writer is a child process (#2179).
func TestTwoProcessesWritingTheListLoseNoRow(t *testing.T) {
	if home := os.Getenv(uidListChildHome); home != "" {
		writeListRows(t, home, 1+rowsPerWriter)
		return
	}
	home := t.TempDir()
	box := New().OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"})
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	box.Close() //nolint:errcheck

	child := exec.Command(os.Args[0], "-test.run=^TestTwoProcessesWritingTheListLoseNoRow$", "-test.count=1")
	child.Env = append(os.Environ(), uidListChildHome+"="+home)
	out := make(chan []byte, 1)
	cerr := make(chan error, 1)
	go func() {
		b, err := child.CombinedOutput()
		out <- b
		cerr <- err
	}()
	writeListRows(t, home, 1)
	if err := <-cerr; err != nil {
		t.Fatalf("the second writer failed: %v\n%s", err, <-out)
	}

	l, err := readUIDListFile(filepath.Join(home, "Maildir", UIDListFileName))
	if err != nil {
		t.Fatal(err)
	}
	have := make(map[uint32]bool, len(l.records))
	for _, rec := range l.records {
		have[rec.uid] = true
	}
	missing := 0
	for uid := uint32(1); uid <= 2*rowsPerWriter; uid++ {
		if !have[uid] {
			missing++
		}
	}
	if missing != 0 {
		t.Errorf("the list lost %d of %d rows written by two processes", missing, 2*rowsPerWriter)
	}
}

// A writer that dies mid-write leaves its unique temp, which nobody reuses; the
// next write clears old ones, and leaves a young one that may be a writer's,
// including an old pod's fixed name during a rollout (#2179).
func TestTheNextListWriteClearsOldTemps(t *testing.T) {
	home := t.TempDir()
	box := New().OpenUser(&mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"})
	defer box.Close() //nolint:errcheck
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	list := filepath.Join(home, "Maildir", UIDListFileName)
	old := time.Now().Add(-mailbox.StaleTemp - time.Hour)
	for _, c := range []struct {
		name string
		old  bool
	}{
		{UIDListFileName + ".tmp.1234567", true},
		{UIDListFileName + ".tmp", true},
		{UIDListFileName + ".tmp.7654321", false},
	} {
		p := filepath.Join(home, "Maildir", c.name)
		if err := os.WriteFile(p, []byte("3 V1 N1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if c.old {
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}

	u := box.(*userMailbox)
	if err := writeRowsAsReconcile(u, "INBOX", []listEntry{{uid: 1, filename: "1700000000.M1P1.host,S=4,W=4:2,"}}); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		gone bool
	}{
		{UIDListFileName + ".tmp.1234567", true},
		{UIDListFileName + ".tmp", true},
		{UIDListFileName + ".tmp.7654321", false},
	} {
		_, err := os.Stat(filepath.Join(home, "Maildir", c.name))
		if c.gone && !os.IsNotExist(err) {
			t.Errorf("the old temp %s survived the next write: %v", c.name, err)
		}
		if !c.gone && err != nil {
			t.Errorf("the young temp %s was taken: %v", c.name, err)
		}
	}
	if _, err := os.Stat(list); err != nil {
		t.Fatal(err)
	}
}
