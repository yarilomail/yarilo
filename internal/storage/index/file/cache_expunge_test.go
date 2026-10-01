package file

import (
	"os"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// cachedFolder holds n messages, those in cached with a cache record each; any
// size of cache may be purged unless opts say otherwise.
func cachedFolder(t *testing.T, pct, n int, cached []uint32, opts ...Option) (*userIndex, uint64) {
	t.Helper()
	opts = append([]Option{WithCachePurgeDeletePercentage(pct), WithCachePurgeMinSize(0)}, opts...)
	ui := New(opts...).OpenUser(&mailbox.UserInfo{Username: testUser, Home: t.TempDir()}).(*userHandle).ui
	f, err := ui.OpenFolder("INBOX", 7, "")
	if err != nil {
		t.Fatal(err)
	}
	for uid := uint32(1); uid <= uint32(n); uid++ {
		if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid}); err != nil {
			t.Fatal(err)
		}
	}
	if len(cached) == 0 {
		return ui, f.ID
	}
	indexID, resetID, _, err := ui.CachePairIdentity(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := ui.CachePath(f.ID)
	cf, err := mailindex.CreateCache(path, indexID, resetID)
	if err != nil {
		t.Fatal(err)
	}
	fid, err := cf.AddFields([]mailindex.CacheField{{Name: "probe", Type: mailindex.CacheFieldVariableSize, Decision: mailindex.CacheDecisionYes}})
	if err != nil {
		t.Fatal(err)
	}
	offsets := map[uint32]uint32{}
	for _, uid := range cached {
		if offsets[uid], err = cf.AppendRecord(0, []mailindex.CacheFieldValue{{FieldID: fid, Data: []byte{byte(uid)}}}); err != nil {
			t.Fatal(err)
		}
	}
	_ = cf.Close()
	if err := ui.SetCacheOffsets(f.ID, stampsOf(offsets)); err != nil {
		t.Fatal(err)
	}
	return ui, f.ID
}

// cacheHeader reads the folder's cache header under the pair's current identity.
func cacheHeader(t *testing.T, ui *userIndex, id uint64) mailindex.CacheHeader {
	t.Helper()
	indexID, resetID, _, err := ui.CachePairIdentity(id)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := ui.CachePath(id)
	cf, err := mailindex.OpenCache(path, indexID, resetID)
	if err != nil {
		t.Fatalf("the cache does not open under the index's identity: %v", err)
	}
	defer cf.Close() //nolint:errcheck
	return cf.Header()
}

func expungeByTx(ui *userIndex, id uint64, uids ...uint32) error {
	tx, err := ui.Begin(id)
	if err != nil {
		return err
	}
	for _, uid := range uids {
		tx.Expunge(uid)
	}
	_, err = tx.Commit()
	return err
}

func expungeOneByOne(ui *userIndex, id uint64, uids ...uint32) error {
	for _, uid := range uids {
		if err := ui.ExpungeMessage(id, uid); err != nil {
			return err
		}
	}
	return nil
}

// An expunge reaches the cache once it is in the log: the records it had move
// from the live count to the deleted one, as the reference counts them.
func TestTheCacheLearnsOfExpungedRecords(t *testing.T) {
	for _, tc := range []struct {
		name    string
		expunge func(*userIndex, uint64, ...uint32) error
	}{
		{"one commit", expungeByTx},
		{"one expunge at a time", expungeOneByOne},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ui, id := cachedFolder(t, -1, 6, []uint32{1, 2, 3, 4})
			if err := tc.expunge(ui, id, 3, 4, 5); err != nil {
				t.Fatal(err)
			}
			if h := cacheHeader(t, ui, id); h.DeletedRecordCount != 2 || h.RecordCount != 2 {
				t.Errorf("deleted %d, records %d; want 2 and 2: uid 5 had no record", h.DeletedRecordCount, h.RecordCount)
			}
		})
	}
}

// A commit the log refuses never happened, for the cache too.
func TestARefusedExpungeLeavesTheCacheCounts(t *testing.T) {
	ui, id := cachedFolder(t, -1, 3, []uint32{1, 2, 3})
	ui.mu.Lock()
	fs := ui.open[id]
	ui.mu.Unlock()
	fs.mu.Lock()
	logPath := fs.indexPath + ".log"
	if fs.logFD != nil {
		_ = fs.logFD.Close()
		fs.logFD = nil
	}
	fs.mu.Unlock()
	if err := os.RemoveAll(logPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(logPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := expungeByTx(ui, id, 1); err == nil {
		t.Fatal("the expunge was not refused, so this row proves nothing")
	}
	if h := cacheHeader(t, ui, id); h.DeletedRecordCount != 0 || h.RecordCount != 3 {
		t.Errorf("deleted %d, records %d after a refused expunge; want 0 and 3", h.DeletedRecordCount, h.RecordCount)
	}
}

// Records stamped into a cache file that is gone have nothing to count, and the
// expunge creates no file.
func TestAnExpungeWithoutACacheIsNotAnError(t *testing.T) {
	ui, id := cachedFolder(t, 20, 3, []uint32{1, 2, 3})
	path, _ := ui.CachePath(id)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := expungeByTx(ui, id, 1, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("an expunge created the cache file: %v", err)
	}
}

// The deleted share purges the cache once it reaches the threshold, and the
// new generation starts with nothing deleted.
func TestTheDeletedShareDecidesThePurge(t *testing.T) {
	for _, tc := range []struct {
		name        string
		pct         int
		msgs        uint32
		expunge     []uint32
		wantPurge   bool
		wantDeleted uint32
		wantRecords uint32
	}{
		{name: "below: 1 of 10 is 10%", pct: 20, expunge: []uint32{1}, wantDeleted: 1, wantRecords: 9},
		{name: "at: 2 of 10 is 20%", pct: 20, expunge: []uint32{1, 2}, wantPurge: true, wantRecords: 8},
		{name: "below: 2 of 11 is 18% of all records, 22% of the live ones", pct: 20, msgs: 11, expunge: []uint32{1, 2}, wantDeleted: 2, wantRecords: 9},
		{name: "never: half gone, -1 keeps it", pct: -1, expunge: []uint32{1, 2, 3, 4, 5}, wantDeleted: 5, wantRecords: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.msgs == 0 {
				tc.msgs = 10
			}
			ui, id := cachedFolder(t, tc.pct, int(tc.msgs), uidsUpTo(tc.msgs))
			before := cacheHeader(t, ui, id).FileSeq
			if err := expungeByTx(ui, id, tc.expunge...); err != nil {
				t.Fatal(err)
			}
			h := cacheHeader(t, ui, id)
			if purged := h.FileSeq != before; purged != tc.wantPurge {
				t.Errorf("purged %v, want %v", purged, tc.wantPurge)
			}
			if h.DeletedRecordCount != tc.wantDeleted || h.RecordCount != tc.wantRecords {
				t.Errorf("deleted %d, records %d; want %d and %d", h.DeletedRecordCount, h.RecordCount, tc.wantDeleted, tc.wantRecords)
			}
		})
	}
}

func uidsUpTo(n uint32) []uint32 {
	out := make([]uint32, 0, n)
	for uid := uint32(1); uid <= n; uid++ {
		out = append(out, uid)
	}
	return out
}

// A cache smaller than the minimum is not purged whatever share is gone; the
// same file at the minimum is.
func TestASmallCacheIsNotPurged(t *testing.T) {
	for _, tc := range []struct {
		name      string
		above     int64
		wantPurge bool
	}{
		{"one byte short of the minimum", 1, false},
		{"at the minimum", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ui, id := cachedFolder(t, 20, 10, uidsUpTo(10))
			path, _ := ui.CachePath(id)
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			ui.b.cachePurgeMinSize = st.Size() + tc.above
			before := cacheHeader(t, ui, id).FileSeq
			if err := expungeByTx(ui, id, 1, 2, 3, 4, 5); err != nil {
				t.Fatal(err)
			}
			if purged := cacheHeader(t, ui, id).FileSeq != before; purged != tc.wantPurge {
				t.Errorf("half gone, file at the minimum minus %d: purged %v, want %v", tc.above, purged, tc.wantPurge)
			}
		})
	}
}

// appendContinued writes one new head and chains n records behind it, so the
// cache carries n continued records.
func appendContinued(t *testing.T, ui *userIndex, id uint64, n int) {
	t.Helper()
	indexID, resetID, _, err := ui.CachePairIdentity(id)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := ui.CachePath(id)
	cf, err := mailindex.OpenCache(path, indexID, resetID)
	if err != nil {
		t.Fatal(err)
	}
	defer cf.Close() //nolint:errcheck
	prev := uint32(0)
	for i := 0; i < n; i++ {
		off, err := cf.AppendRecord(prev, []mailindex.CacheFieldValue{{FieldID: 0, Data: []byte{byte(i)}}})
		if err != nil {
			t.Fatal(err)
		}
		if prev == 0 {
			off, err = cf.AppendRecord(off, []mailindex.CacheFieldValue{{FieldID: 0, Data: []byte{byte(i)}}})
			if err != nil {
				t.Fatal(err)
			}
		}
		prev = off
	}
}

// Continued records past the share purge the cache, as the reference does, with
// live records alone as the base; one fewer leaves it.
func TestTheContinuedShareDecidesThePurge(t *testing.T) {
	for _, tc := range []struct {
		name      string
		continued int
		wantPurge bool
	}{
		{"one short: 17 over 9 messages is 188%", 17, false},
		{"at: 18 over 9 messages is 200%", 18, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ui, id := cachedFolder(t, -1, 10, uidsUpTo(10), WithCachePurgeContinuedPercentage(200))
			appendContinued(t, ui, id, tc.continued)
			h := cacheHeader(t, ui, id)
			if h.ContinuedRecordCount != uint32(tc.continued) {
				t.Fatalf("the cache carries %d continued records, want %d", h.ContinuedRecordCount, tc.continued)
			}
			if err := expungeByTx(ui, id, 1); err != nil {
				t.Fatal(err)
			}
			if purged := cacheHeader(t, ui, id).FileSeq != h.FileSeq; purged != tc.wantPurge {
				t.Errorf("%d continued over 9 messages: purged %v, want %v", tc.continued, purged, tc.wantPurge)
			}
		})
	}
}
