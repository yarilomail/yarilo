package mailbox

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A temp dated to an old INTERNALDATE just before publishing has an old mtime
// and a fresh ctime; no sweep in any process may take it (#2175). Once both
// are old, it is a crash's leftover and goes.
func TestATempDatedBeforePublishingIsNotStale(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, ".temp.1")
	if err := os.WriteFile(file, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2024, 11, 23, 17, 45, 9, 0, time.UTC)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}

	if removed, err := SweepStaleTemps(dir, ".temp."); err != nil || len(removed) != 0 {
		t.Fatalf("swept %v (err %v): a fresh ctime is a save about to publish", removed, err)
	}

	defer SetSweepClock(func() time.Time { return time.Now().Add(StaleTemp + time.Hour) })()
	if removed, err := SweepStaleTemps(dir, ".temp."); err != nil || len(removed) != 1 {
		t.Errorf("swept %v (err %v), want the temp once its ctime is old too", removed, err)
	}
}
