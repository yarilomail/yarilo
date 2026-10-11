package maildir

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
)

// Saves and reconciles on one folder at once, from one mailbox object and from
// a second: every path takes the list, then the mailbox, then the journal, so
// none waits on another that waits on it. An inversion shows as a wait that
// runs out (#2184).
func TestSavesAndReconcilesNeverWaitOnEachOther(t *testing.T) {
	defer SetUIDListLockWait(2 * time.Second)()
	home, box, idx, folder := windowFolder(t)
	other, otherIdx := openWindowUser(t, home)
	otherFolder, err := otherIdx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}

	const rounds = 100
	var wg sync.WaitGroup
	errs := make(chan error, 3*rounds)
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			// A file of an external MDA each round, so every pass applies
			// something: a clean folder never takes a lock.
			name := filepath.Join(box.folderPath("INBOX"), "new", fmt.Sprintf("1700000%03d.M1P1.mda", i))
			if err := os.WriteFile(name, []byte("From: a@b\r\n\r\nz\r\n"), 0o600); err != nil {
				errs <- err
				continue
			}
			f, err := idx.OpenFolder("INBOX", folder.UIDValidity)
			if err == nil {
				_, err = box.ReconcileIndex(mailboxbase.Open(box, idx), idx, f)
			}
			if err != nil {
				errs <- err
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			if err := deliverOneErr(box, idx, folder); err != nil {
				errs <- err
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			if err := deliverOneErr(other, otherIdx, otherFolder); err != nil {
				errs <- err
			}
		}
	}()
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(60 * time.Second):
		t.Fatal("saves and reconciles did not finish in 60s: two paths wait on each other")
	}
	close(errs)
	for err := range errs {
		t.Errorf("a save or a reconcile failed beside the others: %v", err)
	}
}
