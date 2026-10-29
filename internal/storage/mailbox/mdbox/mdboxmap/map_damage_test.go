package mdboxmap

import (
	"os"
	"path/filepath"
	"testing"
)

func reopen(t *testing.T, dir string) *Map {
	t.Helper()
	m, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// A refcount past the field's width stays at the top, live and on replay: a
// wrap would bring it near zero, and a purge would reclaim a live message.
func TestARefcountPastItsWidthSaturates(t *testing.T) {
	m, dir := openTestMap(t)
	uid := seedRecord(t, m, 1)
	for range 3 {
		if err := m.UpdateRefcounts([]uint32{uid}, 30000); err != nil {
			t.Fatal(err)
		}
	}
	if got := refcountOf(t, m, uid); got != 0xFFFF {
		t.Errorf("live refcount %d, want 65535", got)
	}
	_ = m.Close()
	if got := refcountOf(t, reopen(t, dir), uid); got != 0xFFFF {
		t.Errorf("replayed refcount %d, want 65535", got)
	}
}

// A journal another map wrote is dropped at open: the base's records stay, the
// journal's do not, and the journal is gone.
func TestAForeignJournalIsDroppedAtOpen(t *testing.T) {
	m, dir := openTestMap(t)
	seedRecord(t, m, 1)
	if err := m.Compact(); err != nil {
		t.Fatal(err)
	}
	foreignID := m.indexID + 1
	_ = m.Close()
	seedLogAppend(t, dir, foreignID, MapEntry{UID: 50, FileID: 9, Size: 10, RefCount: 1})

	again := reopen(t, dir)
	if n := again.MessageCount(); n != 1 {
		t.Errorf("%d records after dropping a foreign journal, want the base's 1", n)
	}
	if _, ok, _ := again.Lookup(50); ok {
		t.Error("a record from the foreign journal was applied")
	}
	if _, err := os.Stat(filepath.Join(dir, MapIndexFileName) + ".log"); !os.IsNotExist(err) {
		t.Errorf("the foreign journal is still there: %v", err)
	}
}

// A journal still holding what the base already folded in, as a crash between
// the base write and the journal's removal leaves it, adds nothing twice.
func TestAJournalRecordTheBaseHoldsIsNotAppliedTwice(t *testing.T) {
	m, dir := openTestMap(t)
	uid := seedRecord(t, m, 1)
	if err := m.Compact(); err != nil {
		t.Fatal(err)
	}
	e, _, err := m.Lookup(uid)
	if err != nil {
		t.Fatal(err)
	}
	id := m.indexID
	_ = m.Close()
	seedLogAppend(t, dir, id, e)

	if n := reopen(t, dir).MessageCount(); n != 1 {
		t.Errorf("%d records, want 1", n)
	}
}

// A torn journal tail is where the replay stops: what came before it reads.
func TestATornJournalTailStopsTheReplayThere(t *testing.T) {
	m, dir := openTestMap(t)
	first := seedRecord(t, m, 1)
	second := seedRecord(t, m, 2)
	_ = m.Close()
	logPath := filepath.Join(dir, MapIndexFileName) + ".log"
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// A frame header claiming more than the 16 MB a record may be, then junk.
	if _, err := f.Write([]byte{0xff, 0xff, 0xff, 0x7f, 0x01, 0, 0, 0, 1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	again := reopen(t, dir)
	for _, uid := range []uint32{first, second} {
		if _, ok, err := again.Lookup(uid); err != nil || !ok {
			t.Errorf("uid %d before the torn tail: ok=%v err=%v", uid, ok, err)
		}
	}
}

// A base shorter than its header claims is refused, not read as fewer records.
func TestABaseShorterThanItsHeaderIsRefused(t *testing.T) {
	m, dir := openTestMap(t)
	for i := uint32(1); i <= 4; i++ {
		seedRecord(t, m, i)
	}
	if err := m.Compact(); err != nil {
		t.Fatal(err)
	}
	_ = m.Close()
	base := filepath.Join(dir, MapIndexFileName)
	raw, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base, raw[:len(raw)-baseRecordLen], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, "alice@example.com"); err == nil {
		t.Error("a base missing a record it claims opened")
	}
}
