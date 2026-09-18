package maildir

import (
	"os"
	"path/filepath"
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

// Reading the list costs one open and one fstat on that descriptor: the two
// path walks it used to take are what the hot path pays for per read (#1875).
func TestReadingTheListTakesNoPathStat(t *testing.T) {
	box := batchBox(t)
	if err := os.WriteFile(box.uidListPath("INBOX"), []byte("3 V1 N2 G0\n1 :a.host,S=5:2,\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stats atomic.Int64
	real := statPath
	statPath = func(name string) (os.FileInfo, error) {
		stats.Add(1)
		return real(name)
	}
	t.Cleanup(func() { statPath = real })

	if _, err := box.readUIDList("INBOX"); err != nil {
		t.Fatal(err)
	}
	if got := stats.Load(); got != 0 {
		t.Errorf("reading the list made %d path stats, want 0", got)
	}
}
