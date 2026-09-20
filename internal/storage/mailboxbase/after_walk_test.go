package mailboxbase_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
)

// deliverToNew puts a message where a delivery puts it and settles both
// directories, so the only change the next walk can see is its own.
func deliverToNew(t *testing.T, inbox, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(inbox, "new", name), []byte("Subject: x\r\n\r\nbody\r\n"), 0o600); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	old := time.Now().Add(-time.Hour)
	for _, sub := range []string{"cur", "new"} {
		if err := os.Chtimes(filepath.Join(inbox, sub), old, old); err != nil {
			t.Fatalf("settle %s: %v", sub, err)
		}
	}
}

// A hot arrival directory is never held, so repeated opens after a delivery do
// walk -- but they walk the arrivals alone. What must not happen is reading the
// whole store each time for a folder whose cur/ has not moved (#1875).
func TestAWalkDoesNotForceTheNextOne(t *testing.T) {
	box, inbox := gateSetup(t)
	deliverToNew(t, inbox, "1700000900.M1P1.h")

	if _, err := box.Folder("INBOX", 0); err != nil {
		t.Fatalf("first open: %v", err)
	}
	scanned := syncCount(t, "scanned")
	for i := 0; i < 5; i++ {
		if _, err := box.Folder("INBOX", 0); err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
	}
	if got := syncCount(t, "scanned") - scanned; got != 0 {
		t.Errorf("five opens after a delivery read the whole store %v times", got)
	}
	if n := messageCount(t, box); n != 1 {
		t.Errorf("the folder holds %d messages, want 1", n)
	}

	// Once the arrival directory settles and the window passes, one full walk
	// is owed -- the folder really was written to, and the reference owes it
	// too. That walk is what ever takes an out-of-band removal.
	scanned = syncCount(t, "scanned")
	settle(t, inbox, time.Now().Add(-2*time.Second))
	time.Sleep(1100 * time.Millisecond)
	if _, err := box.Folder("INBOX", 0); err != nil {
		t.Fatalf("open after the window: %v", err)
	}
	if got := syncCount(t, "scanned") - scanned; got != 1 {
		t.Errorf("the owed full walk ran %v times, want 1", got)
	}
}

// The counter-row: a change that lands while the walk is running is not lost
// by storing the token after it. The directory is dirty when the token is
// re-read, so the window owes one more walk and that walk takes the message.
func TestAChangeDuringTheWalkIsNotLost(t *testing.T) {
	box, inbox := gateSetup(t)
	deliverToNew(t, inbox, "1700001000.M1P1.h")

	landed := false
	defer mailboxbase.SetAfterWalk(func(string) {
		if landed {
			return
		}
		landed = true
		// Straight into cur/, as another MUA does, in the window between the
		// walk and the re-read of the token.
		if err := os.WriteFile(filepath.Join(inbox, "cur", "1700001001.M2P2.h:2,"),
			[]byte("Subject: late\r\n\r\nbody\r\n"), 0o600); err != nil {
			t.Errorf("late delivery: %v", err)
		}
	})()

	if _, err := box.Folder("INBOX", 0); err != nil {
		t.Fatalf("first open: %v", err)
	}
	if !landed {
		t.Fatal("the seam never fired, so nothing landed during the walk")
	}
	if n := messageCount(t, box); n != 1 {
		t.Fatalf("the first walk holds %d messages, want the one it could see", n)
	}

	// The owed walk, once the window has passed: it takes what landed.
	time.Sleep(1100 * time.Millisecond)
	if _, err := box.Folder("INBOX", 0); err != nil {
		t.Fatalf("second open: %v", err)
	}
	if n := messageCount(t, box); n != 2 {
		t.Errorf("the folder holds %d messages, want 2: the change that landed during the walk was lost", n)
	}
}
