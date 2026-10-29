package mailops

import (
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/quota"
)

func TestAfterDeltaAppliesTheChange(t *testing.T) {
	for _, tc := range []struct {
		name                string
		dBytes, dMessages   int64
		wantBytes, wantMsgs int64
	}{
		{"a delivery adds", 4096, 1, 104096, 11},
		{"an expunge subtracts", -4096, -1, 95904, 9},
		{"more removed than counted cannot go below zero", -1 << 40, -100, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &Quota{cacheUsage: quota.Usage{StorageBytes: 100000, Messages: 10}, cacheAt: time.Now()}
			got, ok := q.afterDelta(tc.dBytes, tc.dMessages)
			if !ok {
				t.Fatal("a fresh cache refused to answer, so every commit falls back to the sweep")
			}
			if got.StorageBytes != tc.wantBytes || got.Messages != tc.wantMsgs {
				t.Errorf("usage %+v, want %d bytes and %d messages", got, tc.wantBytes, tc.wantMsgs)
			}
		})
	}
}

func TestAfterDeltaRefusesAStaleCache(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		{"never counted", time.Time{}},
		{"counted longer ago than the TTL", time.Now().Add(-2 * cacheTTL)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &Quota{cacheUsage: quota.Usage{StorageBytes: 100000}, cacheAt: tc.at}
			if _, ok := q.afterDelta(4096, 1); ok {
				t.Error("built on a cache it should not trust")
			}
		})
	}
}

// Refreshing the timestamp per delta would keep the total alive under load and
// hide another session's change; it must age out one TTL after the last count.
func TestTheDeltaDoesNotExtendTheCacheLifetime(t *testing.T) {
	at := time.Now().Add(-cacheTTL / 2)
	q := &Quota{cacheUsage: quota.Usage{StorageBytes: 100000, Messages: 10}, cacheAt: at}
	for i := 0; i < 5; i++ {
		if _, ok := q.afterDelta(-100, -1); !ok {
			t.Fatalf("refused at iteration %d", i)
		}
	}
	if !q.cacheAt.Equal(at) {
		t.Errorf("the cache timestamp moved from %v to %v", at, q.cacheAt)
	}
}

// The imapsieve expunge arrives without a size; carrying the total forward
// there would drift with nothing bounding it.
func TestAnUnknownSizeLeavesNothingToBuildOn(t *testing.T) {
	q := &Quota{cacheUsage: quota.Usage{StorageBytes: 100000, Messages: 10}, cacheAt: time.Now()}
	q.committed(false, 0, -1)
	if _, ok := q.afterDelta(-100, -1); ok {
		t.Error("the cache survived a change of unknown size")
	}
}
