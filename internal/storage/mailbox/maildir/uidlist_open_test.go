package maildir

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// A store copied in from the reference is adopted on the first read, without a
// migration step: its list becomes ours (#1593).
func TestACopiedInListIsAdoptedWhenOursIsAbsent(t *testing.T) {
	box := batchBox(t)
	// A store copied in has no list of ours yet; Create wrote an empty one.
	if err := os.Remove(box.uidListPath("INBOX")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	legacy := filepath.Join(box.folderPath("INBOX"), LegacyUIDListFileName)
	if err := os.WriteFile(legacy, []byte("3 V7 N9 G0\n8 :1700000000.M1P1.host,S=10:2,S\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := box.readUIDList("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 {
		t.Fatalf("the copied-in list was not read: %v", m)
	}
	if _, err := os.Stat(box.uidListPath("INBOX")); err != nil {
		t.Errorf("the list was not adopted under our name: %v", err)
	}
}

// Once ours exists, a list appearing under the other name is not looked at:
// adoption is for a store copied in before we wrote, not a running one — the
// decision recorded in #1593 and kept here (#1875).
func TestTheOtherNameIsNotReadOnceOursExists(t *testing.T) {
	box := batchBox(t)
	ours := box.uidListPath("INBOX")
	if err := os.WriteFile(ours, []byte("3 V1 N2 G0\n1 :ours.host,S=5:2,\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(box.folderPath("INBOX"), LegacyUIDListFileName)
	if err := os.WriteFile(legacy, []byte("3 V9 N9 G0\n5 :theirs.host,S=5:2,\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := box.readUIDList("INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m["theirs.host,S=5"]; ok {
		t.Error("a list written under the other name was picked up after ours existed")
	}
	if uid := m["ours.host,S=5"]; uid != 1 {
		t.Errorf("our own list was not the one read: %v", m)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("the other file was moved or removed: %v", err)
	}
}

// What a read costs: a hit asks the filesystem once and opens nothing, a miss
// opens once. Knowing whether another process changed the file cannot be done
// without asking, so one walk is the floor, not a regression (#1875).
func TestAListHitAsksOnceAndOpensNothing(t *testing.T) {
	box := batchBox(t)
	if err := os.WriteFile(box.uidListPath("INBOX"), []byte("3 V1 N2 G0\n1 :a.host,S=5:2,\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Warm the cache: this read is the miss.
	if _, err := box.readUIDList("INBOX"); err != nil {
		t.Fatal(err)
	}

	stats, opens := countFileOps(t)
	if _, err := box.readUIDList("INBOX"); err != nil {
		t.Fatal(err)
	}
	if got := stats.Load(); got != 1 {
		t.Errorf("a cache hit walked the path %d times, want 1", got)
	}
	if got := opens.Load(); got != 0 {
		t.Errorf("a cache hit opened the file %d times, want 0", got)
	}
}

// And a miss opens once -- not twice, which a stat-then-open shape costs.
func TestAListMissOpensOnce(t *testing.T) {
	box := batchBox(t)
	if err := os.WriteFile(box.uidListPath("INBOX"), []byte("3 V1 N2 G0\n1 :a.host,S=5:2,\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stats, opens := countFileOps(t)
	if _, err := box.readUIDList("INBOX"); err != nil {
		t.Fatal(err)
	}
	if got := opens.Load(); got != 1 {
		t.Errorf("a miss opened the file %d times, want 1", got)
	}
	if got := stats.Load(); got > 1 {
		t.Errorf("a miss walked the path %d times, want at most 1", got)
	}
}

// countFileOps swaps the seams for the duration of one row.
func countFileOps(t *testing.T) (*atomic.Int64, *atomic.Int64) {
	t.Helper()
	var stats, opens atomic.Int64
	realStat, realOpen := statPath, openPath
	statPath = func(name string) (os.FileInfo, error) {
		stats.Add(1)
		return realStat(name)
	}
	openPath = func(name string) (*os.File, error) {
		opens.Add(1)
		return realOpen(name)
	}
	t.Cleanup(func() { statPath, openPath = realStat, realOpen })
	return &stats, &opens
}

// The counting row above is only worth its name while every path walk goes
// through the seam: a bare os.Stat would pass it green (#1875).
func TestEveryPathWalkGoesThroughTheSeam(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		checked++
		body, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatal(rerr)
		}
		for n, line := range strings.Split(string(body), "\n") {
			if strings.Contains(line, "= os.Stat") || strings.Contains(line, "= os.Lstat") || strings.Contains(line, "= os.Open") {
				continue
			}
			if strings.Contains(line, "os.Stat(") || strings.Contains(line, "os.Lstat(") || strings.Contains(line, "os.Open(") {
				t.Errorf("%s:%d touches the filesystem outside the seam: %s", f, n+1, strings.TrimSpace(line))
			}
		}
	}
	if checked < 5 {
		t.Fatalf("read %d files, which cannot be right", checked)
	}
}
