package mailboxbase_test

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
)

func reasonCount(t *testing.T, result, reason string) float64 {
	t.Helper()
	return testutil.ToFloat64(mailboxbase.MetricReconcile.WithLabelValues(result, reason))
}

// A walk's cause is what says whether a restart is expensive: a folder this
// process has never seen walks for a different reason than one whose mtime
// moved, and the counter could not tell them apart (#1875).
func TestAWalkRecordsWhatDroveIt(t *testing.T) {
	box, inbox := gateSetup(t)
	settle(t, inbox, time.Now().Add(-time.Hour))

	first := reasonCount(t, "scanned", "first-seen")
	if _, err := box.Folder("INBOX", 0); err != nil {
		t.Fatalf("first open: %v", err)
	}
	if got := reasonCount(t, "scanned", "first-seen") - first; got != 1 {
		t.Errorf("the first open of a folder counted %v first-seen walks, want 1", got)
	}

	// Again with nothing changed: a skip, and a skip carries no reason.
	skipped := reasonCount(t, "skipped", "")
	if _, err := box.Folder("INBOX", 0); err != nil {
		t.Fatalf("second open: %v", err)
	}
	if got := reasonCount(t, "skipped", "") - skipped; got != 1 {
		t.Errorf("the unchanged open counted %v skips, want 1", got)
	}

	// A change from outside: the token moves, and that is what is recorded.
	moved := reasonCount(t, "scanned", "token-moved")
	deliverOutOfBand(t, inbox, "1700000500.M1P1.h:2,", time.Now().Add(-time.Hour))
	if _, err := box.Folder("INBOX", 0); err != nil {
		t.Fatalf("third open: %v", err)
	}
	if got := reasonCount(t, "scanned", "token-moved") - moved; got != 1 {
		t.Errorf("the changed open counted %v token-moved walks, want 1", got)
	}
	if got := reasonCount(t, "scanned", "first-seen") - first; got != 1 {
		t.Errorf("a later walk was counted as first-seen: %v", got)
	}
}
