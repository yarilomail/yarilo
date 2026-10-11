package maildir

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// windowChildHome, set, makes the test named in windowChildTest the child.
const (
	windowChildHome = "YARILO_WINDOW_CHILD_HOME"
	windowChildTest = "YARILO_WINDOW_CHILD_TEST"
)

// windowPause is how long the child holds its window open.
const windowPause = 300 * time.Millisecond

func waitForFile(t *testing.T, path string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not appear within %s", filepath.Base(path), within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// pauseHere tells the parent the child is in its window, then waits to go on.
func pauseHere(t *testing.T, home string) {
	if err := os.WriteFile(filepath.Join(home, "paused"), nil, 0o600); err != nil {
		t.Error(err)
	}
	waitForFile(t, filepath.Join(home, "go"), 10*time.Second)
}

// windowFolder is a maildir user with one recorded message, reconciled once.
func windowFolder(t *testing.T) (string, *userMailbox, mailbox.UserIndex, *mailbox.Folder) {
	t.Helper()
	home := testHome(t.TempDir(), "u@x.com")
	box, idx := openWindowUser(t, home)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	deliverOne(t, box, idx, folder)
	if _, err := box.ReconcileIndex(mailboxbase.Open(box, idx), idx, folder); err != nil {
		t.Fatal(err)
	}
	return home, box, idx, folder
}

// reconcileWhileChildPauses runs the named test as a child, reconciles while it
// holds its window, then releases it. It returns how long the reconcile took.
func reconcileWhileChildPauses(t *testing.T, test, home string, box *userMailbox, idx mailbox.UserIndex, folder *mailbox.Folder) (mailbox.SyncStats, time.Duration) {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^"+test+"$", "-test.count=1")
	child.Env = append(os.Environ(), windowChildHome+"="+home, windowChildTest+"="+test)
	childErr := make(chan error, 1)
	childOut := make(chan []byte, 1)
	go func() {
		out, err := child.CombinedOutput()
		childOut <- out
		childErr <- err
	}()
	waitForFile(t, filepath.Join(home, "paused"), 10*time.Second)

	type result struct {
		st   mailbox.SyncStats
		took time.Duration
		err  error
	}
	done := make(chan result, 1)
	go func() {
		began := time.Now()
		f, err := idx.OpenFolder("INBOX", folder.UIDValidity)
		var st mailbox.SyncStats
		if err == nil {
			st, err = box.ReconcileIndex(mailboxbase.Open(box, idx), idx, f)
		}
		done <- result{st, time.Since(began), err}
	}()
	time.Sleep(windowPause)
	if err := os.WriteFile(filepath.Join(home, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-childErr; err != nil {
		t.Errorf("the child's pass failed: %v\n%s", err, <-childOut)
	}
	r := <-done
	if r.err != nil {
		t.Errorf("the reconcile stopped: %v", r.err)
	}
	return r.st, r.took
}

func isWindowChild(test string) (string, bool) {
	home := os.Getenv(windowChildHome)
	return home, home != "" && os.Getenv(windowChildTest) == test
}

// A save in another process has named its file and not yet committed the
// record: a reconcile waits for it and then sees the record (#2183, #2184).
func TestAReconcileDuringASaveStopsNeither(t *testing.T) {
	const name = "TestAReconcileDuringASaveStopsNeither"
	if home, ok := isWindowChild(name); ok {
		box, idx := openWindowUser(t, home)
		folder, err := idx.OpenFolder("INBOX", 0)
		if err != nil {
			t.Fatal(err)
		}
		testAfterAssign = func() { testAfterAssign = nil; pauseHere(t, home) }
		deliverOne(t, box, idx, folder)
		return
	}
	home, box, idx, folder := windowFolder(t)
	st, took := reconcileWhileChildPauses(t, name, home, box, idx, folder)
	if st.Imported != 0 || st.Relinked != 0 {
		t.Errorf("the reconcile changed %+v for a save it should wait for", st)
	}
	if took < windowPause*3/4 {
		t.Errorf("the reconcile took %s: it ran inside the save's window", took)
	}
	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil || len(msgs) != 2 {
		t.Errorf("index = %d records, err = %v; want the two messages once each", len(msgs), err)
	}
}

// A reconcile in another process has written an import's row and not yet its
// record: this reconcile waits, then imports nothing and stops on nothing. With
// the list held per row it imported the file at that uid too (#2183, #2184).
func TestAReconcileDuringAnotherImportStopsNeither(t *testing.T) {
	const name = "TestAReconcileDuringAnotherImportStopsNeither"
	if home, ok := isWindowChild(name); ok {
		box, idx := openWindowUser(t, home)
		folder, err := idx.OpenFolder("INBOX", 0)
		if err != nil {
			t.Fatal(err)
		}
		testAfterImportRows = func() { testAfterImportRows = nil; pauseHere(t, home) }
		if _, err := box.ReconcileIndex(mailboxbase.Open(box, idx), idx, folder); err != nil {
			t.Fatal(err)
		}
		return
	}
	home, box, idx, folder := windowFolder(t)
	// What an external MDA leaves: a file no record or row knows yet.
	if err := os.WriteFile(filepath.Join(box.folderPath("INBOX"), "new", "1700000099.M1P1.mda"), []byte("From: a@b\r\n\r\ny\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, _ := reconcileWhileChildPauses(t, name, home, box, idx, folder)
	if st.Imported != 0 || st.Relinked != 0 {
		t.Errorf("the reconcile changed %+v for an import the other process owns", st)
	}
	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil || len(msgs) != 2 {
		t.Errorf("index = %d records, err = %v; want the two messages once each", len(msgs), err)
	}
}
