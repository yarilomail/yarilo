package maildir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func listingCounts(t *testing.T) (whole, tail, byName, misses float64) {
	t.Helper()
	return testutil.ToFloat64(metricUIDListRead.WithLabelValues("whole")),
		testutil.ToFloat64(metricUIDListRead.WithLabelValues("tail")),
		testutil.ToFloat64(metricDirRead.WithLabelValues("current-name")),
		testutil.ToFloat64(metricListingMiss.WithLabelValues("no-listing"))
}

// The two reads a name costs are counted apart, with the reason for the miss:
// "one read per login" and "one per FETCH" are otherwise one number (#1875).
func TestANameLookupCountsItsReads(t *testing.T) {
	u, _ := item1Folder(t, 5)
	const base = "1700000002.M2P1.host,S=20,W=20:2,"

	whole0, tail0, name0, miss0 := listingCounts(t)
	if _, err := u.currentName("INBOX", maildirBase(base)); err != nil {
		t.Fatal(err)
	}
	whole1, tail1, name1, miss1 := listingCounts(t)

	if name1 != name0+1 {
		t.Errorf("cur/ reads = %v, want one more than %v", name1, name0)
	}
	if miss1 != miss0+1 {
		t.Errorf("no-listing misses = %v, want one more than %v", miss1, miss0)
	}
	if whole1 != whole0 || tail1 != tail0 {
		t.Errorf("naming a file read the list: whole %v->%v tail %v->%v", whole0, whole1, tail0, tail1)
	}

	// The second lookup uses the listing it just read, and counts nothing.
	if _, err := u.currentName("INBOX", maildirBase(base)); err != nil {
		t.Fatal(err)
	}
	if _, _, name2, miss2 := listingCounts(t); name2 != name1 || miss2 != miss1 {
		t.Errorf("the second lookup read again: dir %v->%v miss %v->%v", name1, name2, miss1, miss2)
	}
}

// A directory that changed under the listing is a different miss from having
// no listing at all, and the stand is meant to tell them apart.
func TestAChangedDirectoryIsAStaleMiss(t *testing.T) {
	u, home := item1Folder(t, 3)
	const base = "1700000001.M1P1.host,S=20,W=20:2,"
	if _, err := u.currentName("INBOX", maildirBase(base)); err != nil {
		t.Fatal(err)
	}

	was := testutil.ToFloat64(metricListingMiss.WithLabelValues("stale-mtime"))
	// Somebody else's file, which moves cur/'s mtime.
	other := filepath.Join(home, "Maildir", "cur", "1700000090.M90P1.host,S=20,W=20:2,")
	if err := os.WriteFile(other, []byte("From: a@b\r\n\r\nx\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := u.currentName("INBOX", maildirBase(base)); err != nil {
		t.Fatal(err)
	}
	if now := testutil.ToFloat64(metricListingMiss.WithLabelValues("stale-mtime")); now != was+1 {
		t.Errorf("stale-mtime misses = %v, want %v", now, was+1)
	}
}

// Every check of a cache costs a stat, and the number is what says whether
// the check belongs per message or per folder open (#1875).
func TestANameLookupCountsItsStats(t *testing.T) {
	u, _ := item1Folder(t, 4)
	dir0 := testutil.ToFloat64(metricCacheStat.WithLabelValues("dir"))
	list0 := testutil.ToFloat64(metricCacheStat.WithLabelValues("list"))
	if _, err := u.RecordPath("INBOX", &mailbox.MessageMeta{UID: 2}); err != nil {
		t.Fatal(err)
	}
	dir1 := testutil.ToFloat64(metricCacheStat.WithLabelValues("dir")) - dir0
	list1 := testutil.ToFloat64(metricCacheStat.WithLabelValues("list")) - list0

	if dir1 < 1 {
		t.Errorf("naming a message stated cur/ %v times, want at least one", dir1)
	}
	if list1 < 1 {
		t.Errorf("naming a message stated the list %v times, want at least one", list1)
	}
	t.Logf("one RecordPath costs stat(dir)=%v stat(list)=%v", dir1, list1)
}

// Inside the window a walk earns, a lookup asks the list no questions: the
// three stats a name cost are two fewer, and the listing keeps its own check.
func TestAWalkEarnsTheListWindow(t *testing.T) {
	u, _ := item1Folder(t, 4)
	if _, err := u.Scan("INBOX"); err != nil {
		t.Fatal(err)
	}
	u.folderCacheFor("INBOX").markChecked()

	list0 := testutil.ToFloat64(metricCacheStat.WithLabelValues("list"))
	dir0 := testutil.ToFloat64(metricCacheStat.WithLabelValues("dir"))
	for uid := uint32(1); uid <= 3; uid++ {
		if _, err := u.RecordPath("INBOX", &mailbox.MessageMeta{UID: uid}); err != nil {
			t.Fatal(err)
		}
	}
	list1 := testutil.ToFloat64(metricCacheStat.WithLabelValues("list")) - list0
	dir1 := testutil.ToFloat64(metricCacheStat.WithLabelValues("dir")) - dir0

	if list1 != 0 {
		t.Errorf("three lookups stated the list %v times inside the window, want 0", list1)
	}
	if dir1 != 3 {
		t.Errorf("the listing was checked %v times, want one per lookup", dir1)
	}
}

// What the window does not cover: a name that moved after the walk is still
// found, because the listing is checked on every lookup (#1800).
func TestANameThatMovedIsStillFound(t *testing.T) {
	u, home := item1Folder(t, 3)
	if _, err := u.Scan("INBOX"); err != nil {
		t.Fatal(err)
	}
	u.folderCacheFor("INBOX").markChecked()
	const base = "1700000002.M2P1.host,S=20,W=20:2,"
	if _, err := u.currentName("INBOX", maildirBase(base)); err != nil {
		t.Fatal(err)
	}

	cur := filepath.Join(home, "Maildir", "cur")
	if err := os.Rename(filepath.Join(cur, base), filepath.Join(cur, base+"S")); err != nil {
		t.Fatal(err)
	}
	got, err := u.currentName("INBOX", maildirBase(base))
	if err != nil {
		t.Fatalf("the moved name is not findable: %v", err)
	}
	if got != base+"S" {
		t.Errorf("currentName says %q, the file is %q", got, base+"S")
	}
}

// A walk keeps its listing only when the mtime has settled: a change inside
// the same tick shares the key and serves a name already gone (#1797).
func TestAWalkKeepsNoListingFromAnUnsettledDirectory(t *testing.T) {
	u, home := item1Folder(t, 3)
	cur := filepath.Join(home, "Maildir", "cur")
	// Written now, so cur/ has just changed and its mtime is inside the window
	// that cannot vouch for its contents.
	if err := os.WriteFile(filepath.Join(cur, "1700000050.M50P1.host,S=20,W=20:2,"),
		[]byte("From: a@b\r\n\r\nx\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Scan("INBOX"); err != nil {
		t.Fatal(err)
	}
	c := u.folderCacheFor("INBOX")
	c.mu.Lock()
	kept := c.entries != nil
	c.mu.Unlock()
	if kept {
		t.Error("the walk kept a listing keyed by an mtime that cannot vouch for it")
	}
}

// Who closes the window has to be visible before anything is changed about
// it: a window shut by our own write costs the next lookup a stat (#1875).
func TestTheWindowSaysWhoClosedIt(t *testing.T) {
	u, _ := item1Folder(t, 3)
	if _, err := u.Scan("INBOX"); err != nil {
		t.Fatal(err)
	}
	c := u.folderCacheFor("INBOX")

	c.markChecked()
	own := testutil.ToFloat64(metricWindowClosed.WithLabelValues("own-write"))
	// Through the save path, so the publish that closes the window runs: a
	// file placed into cur/ by hand never goes through it.
	name, _, _, err := u.Save("INBOX", strings.NewReader("From: a@b\r\n\r\nx\r\n"), 0, 0, nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.AssignUID("INBOX", name, 70); err != nil {
		t.Fatal(err)
	}
	if now := testutil.ToFloat64(metricWindowClosed.WithLabelValues("own-write")); now != own+1 {
		t.Errorf("own-write closings = %v, want %v", now, own+1)
	}

	// A window that is already shut is not shut twice: the number is windows
	// lost, not calls made.
	again := testutil.ToFloat64(metricWindowClosed.WithLabelValues("own-write"))
	c.invalidateDir("own-write")
	if now := testutil.ToFloat64(metricWindowClosed.WithLabelValues("own-write")); now != again {
		t.Errorf("a closed window was counted again: %v -> %v", again, now)
	}
}
