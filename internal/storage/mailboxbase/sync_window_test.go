package mailboxbase_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A burst of commands on a folder that is being written costs one walk, not
// one each: the reference re-walks a directory whose mtime it cannot vouch for
// only once the last check is a window old (maildir-sync.c:625-633). Ours put
// a nonce in the token instead, so every open walked (#1875).
func TestABurstOnADirtyFolderWalksOnce(t *testing.T) {
	box, inbox := gateSetup(t)

	// A delivery this instant: cur/ carries an mtime from the current second,
	// which is what "cannot be vouched for" means.
	deliverNow(t, inbox, "1700000100.M1P1.h:2,")

	scanned := syncCount(t, "scanned")
	held := syncCount(t, "skipped-window")
	// Writes keep landing through the burst, as they do under load: every open
	// sees a token it has not seen. Without the window each of those is a walk.
	for i := 0; i < 30; i++ {
		deliverNow(t, inbox, fmt.Sprintf("1700000101.M%dP1.h:2,", i))
		if _, err := box.Folder("INBOX", 0); err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
	}
	walks := syncCount(t, "scanned") - scanned
	if walks != 1 {
		t.Errorf("thirty opens on a folder written this second walked the store %v times, want 1", walks)
	}
	if got := syncCount(t, "skipped-window") - held; got != 29 {
		t.Errorf("%v opens were held by the window, want 29", got)
	}
}

// Held, not dropped: once the window has passed the folder is walked again,
// which is what keeps a filesystem with second-granularity mtimes honest.
func TestADirtyFolderIsWalkedAgainAfterTheWindow(t *testing.T) {
	box, inbox := gateSetup(t)
	deliverNow(t, inbox, "1700000200.M1P1.h:2,")

	if _, err := box.Folder("INBOX", 0); err != nil {
		t.Fatalf("first open: %v", err)
	}
	scanned := syncCount(t, "scanned")
	if _, err := box.Folder("INBOX", 0); err != nil {
		t.Fatalf("second open: %v", err)
	}
	if got := syncCount(t, "scanned") - scanned; got != 0 {
		t.Fatalf("the window did not hold the second open: %v walks", got)
	}

	// A second later the same dirty folder is walked again: the mtime is now
	// old enough to be compared, and a change inside that second would
	// otherwise never be seen.
	time.Sleep(1100 * time.Millisecond)
	scanned = syncCount(t, "scanned")
	if _, err := box.Folder("INBOX", 0); err != nil {
		t.Fatalf("third open: %v", err)
	}
	if got := syncCount(t, "scanned") - scanned; got != 1 {
		t.Errorf("the folder was walked %v times after the window passed, want 1", got)
	}
}

// deliverNow writes into cur/ and leaves its mtime where the write put it: the
// current second. new/ is backdated, because an arrival directory that is hot
// is never held -- this is the store-writes case (IMAP APPEND, flag renames),
// which is where the stand's walks come from.
func deliverNow(t *testing.T, inbox, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(inbox, "cur", name), []byte("Subject: x\r\n\r\nbody\r\n"), 0o600); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(inbox, "new"), old, old); err != nil {
		t.Fatalf("settle new/: %v", err)
	}
}
