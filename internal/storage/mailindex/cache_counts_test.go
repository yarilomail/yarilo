package mailindex

import (
	"math"
	"path/filepath"
	"testing"
)

// A cache nothing was ever cached in opens with no fields; its counters do not
// wrap below zero, and a deleted count too large to scale reads as all deleted.
func TestCacheCountersStayInRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yarilo.index.cache")
	c, err := CreateCache(path, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	c, err = OpenCache(path, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck
	if n := len(c.Fields()); n != 0 {
		t.Errorf("%d fields in a cache nothing was cached in", n)
	}
	if err := c.ExpungeCount(5); err != nil {
		t.Fatal(err)
	}
	if h := c.Header(); h.RecordCount != 0 {
		t.Errorf("record count %d after expunging more than it held", h.RecordCount)
	}
	h := CacheHeader{DeletedRecordCount: math.MaxUint32 / 50}
	if got := h.DeletePercentage(10); got != math.MaxUint32 {
		t.Errorf("delete percentage %d, want the maximum", got)
	}
}

// An extension intro naming more bytes than its record carries is not decoded.
func TestAnExtIntroNamingBytesItLacksIsRefused(t *testing.T) {
	b := EncodeTxExtIntroPayload(TxExtIntro{Name: "modseq", RecordSize: 8, RecordAlign: 8})
	b[18] = 200
	if _, ok := DecodeTxExtIntroPayload(b); ok {
		t.Error("decoded an intro naming bytes it lacks")
	}
}
