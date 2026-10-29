package mdbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func saveReceivedAt(t *testing.T, u *userMailbox, body string, at time.Time) string {
	t.Helper()
	name, _, _, err := u.SaveReceived("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{}, at)
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func mfiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "m.") {
			out = append(out, e.Name())
		}
	}
	return out
}

// One m-file holding old and new mail splits: the old go to alt, the new are
// rewritten in primary, and every live message still reads.
func TestAltMoveSplitsAFileHoldingOldAndNewMail(t *testing.T) {
	_, u, _ := altTestBackend(t)
	now := time.Now()
	old := saveReceivedAt(t, u, "Subject: old\r\n\r\nold body\r\n", now.Add(-48*time.Hour))
	fresh := saveReceivedAt(t, u, "Subject: new\r\n\r\nnew body\r\n", now)
	gone := saveReceivedAt(t, u, "Subject: gone\r\n\r\nexpunged body\r\n", now.Add(-48*time.Hour))
	if err := u.Remove("INBOX", gone); err != nil {
		t.Fatal(err)
	}
	before := mfiles(t, u.storagePath())
	if len(before) != 1 {
		t.Fatalf("setup wants one m-file, have %v", before)
	}

	stats, err := u.AltMove(AltMoveQuery{Before: now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Moved != 1 || stats.FilesCreated != 2 || stats.FilesUnlinked != 1 {
		t.Errorf("stats %+v, want 1 moved, 2 files created, 1 unlinked", stats)
	}
	if len(stats.MovedFilenames) != 1 || stats.MovedFilenames[0] != old {
		t.Errorf("moved %v, want [%s]", stats.MovedFilenames, old)
	}
	if got := mfiles(t, u.altStoragePath()); len(got) != 1 {
		t.Errorf("alt m-files %v, want one", got)
	}
	after := mfiles(t, u.storagePath())
	if len(after) != 1 || after[0] == before[0] {
		t.Errorf("primary m-files %v, want one new file in place of %v", after, before)
	}
	if b := testFetchBody(t, u, old); !strings.Contains(b, "old body") {
		t.Errorf("old message after split: %q", b)
	}
	if b := testFetchBody(t, u, fresh); !strings.Contains(b, "new body") {
		t.Errorf("new message after split: %q", b)
	}
	// The expunged record is not old mail to keep: it stays with the primary
	// file for purge to reclaim, and never reaches alt.
	if carries(t, u.altStoragePath(), "expunged body") {
		t.Error("alt carries the expunged record")
	}
	if _, err := u.Purge(); err != nil {
		t.Fatal(err)
	}
	if carries(t, u.storagePath(), "expunged body") {
		t.Error("primary carries the expunged record after purge")
	}
	if b := testFetchBody(t, u, fresh); !strings.Contains(b, "new body") {
		t.Errorf("new message after purge: %q", b)
	}

	// A second run finds nothing left to move.
	again, err := u.AltMove(AltMoveQuery{Before: now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if again.Moved != 0 {
		t.Errorf("second run moved %d", again.Moved)
	}
}

// A run with no alt tree yet to read from (reverse before any move) is a
// no-op, not an error.
func TestAReverseAltMoveWithNoAltTreeIsANoOp(t *testing.T) {
	_, u, _ := altTestBackend(t)
	name := testSaveMsg(t, u, "Subject: p\r\n\r\nprimary\r\n")
	if err := os.RemoveAll(u.altStoragePath()); err != nil {
		t.Fatal(err)
	}
	stats, err := u.AltMove(AltMoveQuery{Reverse: true})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Moved != 0 || stats.Candidates != 0 {
		t.Errorf("stats %+v from an empty alt tree", stats)
	}
	if b := testFetchBody(t, u, name); !strings.Contains(b, "primary") {
		t.Errorf("primary message after the no-op: %q", b)
	}
}

func carries(t *testing.T, dir, text string) bool {
	t.Helper()
	for _, f := range mfiles(t, dir) {
		raw, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), text) {
			return true
		}
	}
	return false
}

// A message on alt keeps its arrival date through a move: the date is read from
// whichever tier holds the bytes, not only from primary (#2175).
func TestAMoveOfAnAltMessageKeepsItsArrivalDate(t *testing.T) {
	_, u, _ := altTestBackend(t)
	when := time.Date(2024, 3, 9, 8, 7, 6, 0, time.UTC)
	name := saveReceivedAt(t, u, "Subject: a\r\n\r\nalt body\r\n", when)
	if _, err := u.AltMove(AltMoveQuery{Before: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := u.Create("Archive"); err != nil {
		t.Fatal(err)
	}
	moved, guid, err := u.Move("INBOX", "Archive", name, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if got := scanDate(t, u, guid); !got.Equal(when) {
		t.Errorf("arrival date after the move %v, want %v", got, when)
	}
	if b := testFetchBody(t, u, moved); !strings.Contains(b, "alt body") {
		t.Errorf("moved body %q", b)
	}
}
