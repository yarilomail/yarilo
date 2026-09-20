package mailboxbase_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// arrive puts a message where a delivery puts it, leaving cur/ where it was.
func arrive(t *testing.T, inbox, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(inbox, "new", name), []byte("Subject: x\r\n\r\nbody\r\n"), 0o600); err != nil {
		t.Fatalf("deliver: %v", err)
	}
}

// A delivery with cur/ unmoved is taken by reading the arrivals alone: the
// reference calls this the partial sync (maildir-sync.c:860-867), and it is
// what the hot-arrival case costs today (#1875).
func TestADeliveryIsTakenByReadingTheArrivalsAlone(t *testing.T) {
	box, inbox := gateSetup(t)
	settle(t, inbox, time.Now().Add(-time.Hour))
	if _, err := box.Folder("INBOX", 0); err != nil {
		t.Fatalf("warm the cache: %v", err)
	}

	partial := syncCount(t, "scanned-partial")
	full := syncCount(t, "scanned")
	arrive(t, inbox, "1700002000.M1P1.h")
	if _, err := box.Folder("INBOX", 0); err != nil {
		t.Fatalf("open after the delivery: %v", err)
	}

	if got := syncCount(t, "scanned-partial") - partial; got != 1 {
		t.Errorf("the delivery drove %v partial walks, want 1", got)
	}
	if got := syncCount(t, "scanned") - full; got != 0 {
		t.Errorf("the whole store was walked %v times for a delivery that only touched new/", got)
	}
	if n := messageCount(t, box); n != 1 {
		t.Errorf("the folder holds %d messages, want the one that arrived", n)
	}
}

// The definition, and the row that holds it: a pass that did not read cur/
// must not conclude anything from a name's absence there. A message removed
// out of band survives the partial pass and goes only when a full one sees it.
func TestAPartialPassRemovesNothing(t *testing.T) {
	box, inbox := gateSetup(t)
	deliverOutOfBand(t, inbox, "1700002100.M1P1.h:2,", time.Now().Add(-time.Hour))
	if _, err := box.Folder("INBOX", 0); err != nil {
		t.Fatalf("take the first message: %v", err)
	}
	if n := messageCount(t, box); n != 1 {
		t.Fatalf("the folder holds %d messages before the removal, want 1", n)
	}

	// Gone from cur/ behind everyone's back, with cur/'s mtime put back to the
	// exact value the last walk recorded: a directory that says it has not
	// moved is the only way to reach the partial pass.
	curDir := filepath.Join(inbox, "cur")
	was, err := os.Stat(curDir)
	if err != nil {
		t.Fatalf("stat cur: %v", err)
	}
	if err := os.Remove(filepath.Join(curDir, "1700002100.M1P1.h:2,")); err != nil {
		t.Fatalf("remove out of band: %v", err)
	}
	if err := os.Chtimes(curDir, was.ModTime(), was.ModTime()); err != nil {
		t.Fatalf("restore cur mtime: %v", err)
	}
	arrive(t, inbox, "1700002101.M2P2.h")

	partial := syncCount(t, "scanned-partial")
	if _, err := box.Folder("INBOX", 0); err != nil {
		t.Fatalf("partial open: %v", err)
	}
	if got := syncCount(t, "scanned-partial") - partial; got != 1 {
		t.Fatalf("the open drove %v partial walks, want 1: the row proves nothing otherwise", got)
	}
	if n := messageCount(t, box); n != 2 {
		t.Errorf("the folder holds %d messages after the partial pass, want 2: it judged an absence it never read", n)
	}

	// A full pass, once cur/ says it moved, takes the removal.
	settle(t, inbox, time.Now().Add(-2*time.Second))
	if err := os.Chtimes(filepath.Join(inbox, "cur"), time.Now().Add(-2*time.Second), time.Now().Add(-2*time.Second)); err != nil {
		t.Fatalf("move cur: %v", err)
	}
	if _, err := box.Folder("INBOX", 0); err != nil {
		t.Fatalf("full open: %v", err)
	}
	if n := messageCount(t, box); n != 1 {
		t.Errorf("the folder holds %d messages after the full pass, want 1: the removal was never taken", n)
	}
}
