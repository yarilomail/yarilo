package mdboxmap

import (
	"errors"
	"io"
	"math/bits"
	"os"
	"runtime"
	"syscall"
	"testing"
)

// foldedMap writes n records, folds them into the base and opens a second
// handle on it, which holds the header and nothing else.
func foldedMap(t *testing.T, n int) (*Map, string) {
	t.Helper()
	m, dir := openTestMap(t)
	layouts := make([]RecordLayout, n)
	for i := range layouts {
		layouts[i] = RecordLayout{FileID: uint32(1 + i/100), Offset: uint32(i%100) * 64, Size: 64, GUID: [16]byte{byte(i), byte(i >> 8), 1}}
	}
	if _, err := m.AppendRecords(layouts); err != nil {
		t.Fatal(err)
	}
	if err := m.Compact(); err != nil {
		t.Fatal(err)
	}
	r, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, dir
}

// A folder's worth of lookups is one forward pass, not a search each: the reads
// grow with the blocks the map spans, never with the lookups times log n.
func TestLookupManyReadsTheBaseInOnePass(t *testing.T) {
	const lookups = 10000
	tests := []struct {
		records  int
		maxReads int
	}{
		{2000, 2000/blockRecs + 2},
		{10000, 10000/blockRecs + 2},
	}
	for _, tc := range tests {
		m, _ := foldedMap(t, tc.records)
		uids := make([]uint32, lookups)
		for i := range uids {
			uids[i] = uint32(i%tc.records) + 1
		}
		ResetCounts()
		got, err := m.LookupMany(uids)
		if err != nil {
			t.Fatal(err)
		}
		for i, e := range got {
			if e.UID != uids[i] {
				t.Fatalf("map %d: lookup %d gave map_uid %d, want %d", tc.records, i, e.UID, uids[i])
			}
		}
		reads := BaseReads()
		t.Logf("map %d: %d lookups, %d base reads", tc.records, lookups, reads)
		if reads > tc.maxReads {
			t.Errorf("map %d: %d base reads, want at most %d", tc.records, reads, tc.maxReads)
		}
	}
}

// One lookup probes map_uids alone down to a block, then reads that block.
func TestLookupReadsLogarithmically(t *testing.T) {
	for _, n := range []int{2000, 10000} {
		m, _ := foldedMap(t, n)
		want := bits.Len(uint(n/blockRecs)) + 2
		for _, uid := range []uint32{1, uint32(n / 2), uint32(n)} {
			ResetCounts()
			e, ok, err := m.Lookup(uid)
			if err != nil || !ok || e.UID != uid {
				t.Fatalf("map %d: Lookup(%d) = %+v %v %v", n, uid, e, ok, err)
			}
			if r := BaseReads(); r > want {
				t.Errorf("map %d: Lookup(%d) took %d base reads, want at most %d", n, uid, r, want)
			}
		}
	}
}

// A fold streams base and layer to the new file: what it allocates does not
// grow with the records it copies.
func TestAFoldDoesNotHoldTheMap(t *testing.T) {
	alloc := func(n int) uint64 {
		m, _ := foldedMap(t, n)
		if err := m.UpdateRefcounts([]uint32{1, uint32(n)}, +1); err != nil {
			t.Fatal(err)
		}
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		if err := m.Compact(); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	small, large := alloc(2000), alloc(10000)
	perRecord := (float64(large) - float64(small)) / 8000
	t.Logf("fold allocates %d B at 2000 records, %d B at 10000: %.2f B a record", small, large, perRecord)
	if perRecord > 1 {
		t.Errorf("fold allocates %.2f B a record, want under 1", perRecord)
	}
}

// The digest is checked at the end of every whole walk: a record changed under
// an intact header is an error, not a refcount purge would act on.
func TestAWalkRefusesRecordsTheDigestDoesNot(t *testing.T) {
	m, dir := foldedMap(t, 300)
	f, err := os.OpenFile(dir+"/"+MapIndexFileName, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The refcount of the first record.
	if _, err := f.WriteAt([]byte{9}, baseHeaderLen+16); err != nil {
		t.Fatal(err)
	}
	f.Close() //nolint:errcheck
	if _, err := m.GetZeroRefFiles(); err == nil {
		t.Fatal("a walk over records that do not match the digest succeeded")
	}
}

// A base cut short under an open handle answers an error, not a missing record
// purge is free to reclaim.
func TestAShortBaseReadIsAnError(t *testing.T) {
	m, dir := foldedMap(t, 300)
	if err := os.Truncate(dir+"/"+MapIndexFileName, baseHeaderLen+10*baseRecordLen); err != nil {
		t.Fatal(err)
	}
	_, _, err := m.st.get(299)
	if !errors.Is(err, errBaseRead) {
		t.Fatalf("lookup past the end of a cut base: %v, want errBaseRead", err)
	}
}

// A fold makes the new base durable before the log it replaces goes: the file,
// then the directory holding the rename.
func TestAFoldSyncsTheFileAndItsDirectory(t *testing.T) {
	m, _ := openTestMap(t)
	if _, err := m.AppendRecord(1, 0, 10, [16]byte{1}); err != nil {
		t.Fatal(err)
	}
	var seen []string
	syncSeam = func(what string) { seen = append(seen, what) }
	defer func() { syncSeam = nil }()
	if err := m.Compact(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "file" || seen[1] != "dir" {
		t.Errorf("fold synced %v, want [file dir]", seen)
	}
}

// staleReader answers every read as a replaced NFS file does.
type staleReader struct{}

func (staleReader) ReadAt([]byte, int64) (int, error) { return 0, syscall.ESTALE }

// Another node's fold replaces the base under an open session: a lookup takes
// the new one and answers, and only a read failing again reaches the caller.
func TestALookupOnAReplacedBaseReadsTheNewOne(t *testing.T) {
	lookups := []struct {
		name string
		do   func(m *Map, uid uint32) (MapEntry, error)
	}{
		{"Lookup", func(m *Map, uid uint32) (MapEntry, error) {
			e, _, err := m.Lookup(uid)
			return e, err
		}},
		{"LookupReloaded", func(m *Map, uid uint32) (MapEntry, error) {
			e, _, err := m.LookupReloaded(uid)
			return e, err
		}},
		{"LookupMany", func(m *Map, uid uint32) (MapEntry, error) {
			es, err := m.LookupMany([]uint32{uid})
			if err != nil {
				return MapEntry{}, err
			}
			return es[0], nil
		}},
	}
	for _, lk := range lookups {
		cases := []struct {
			name             string
			replaced, always bool
			wantRef          uint16
		}{
			{"replaced", true, false, 2},
			// stat still shows the old file, as an NFS attribute cache can.
			{"looks-unchanged", false, false, 1},
			{"fails-again", true, true, 0},
		}
		for _, tc := range cases {
			always := tc.always
			t.Run(lk.name+"/"+tc.name, func(t *testing.T) {
				r, dir := foldedMap(t, 300)
				old := r.st.base
				if tc.replaced {
					w, err := Open(dir, "alice@example.com")
					if err != nil {
						t.Fatal(err)
					}
					defer w.Close() //nolint:errcheck
					if err := w.UpdateRefcounts([]uint32{150}, +1); err != nil {
						t.Fatal(err)
					}
					if err := w.Compact(); err != nil {
						t.Fatal(err)
					}
				}
				readSeam = func(b *baseRecords) io.ReaderAt {
					if always || b == old {
						return staleReader{}
					}
					return b.r
				}
				defer func() { readSeam = nil }()
				e, err := lk.do(r, 150)
				if always {
					if !errors.Is(err, errBaseRead) {
						t.Fatalf("a base failing again answered %+v %v, want errBaseRead", e, err)
					}
					return
				}
				if err != nil || e.UID != 150 || e.RefCount != tc.wantRef {
					t.Fatalf("lookup on a failing base: %+v %v, want map_uid 150 at refcount %d", e, err, tc.wantRef)
				}
			})
		}
	}
}

// A walk that fails is an error, not a short list of purge candidates.
func TestCompactGarbageReturnsAFailedWalk(t *testing.T) {
	m, _ := foldedMap(t, 300)
	readSeam = func(*baseRecords) io.ReaderAt { return staleReader{} }
	defer func() { readSeam = nil }()
	if got, err := m.CompactGarbage(); !errors.Is(err, errBaseRead) {
		t.Fatalf("CompactGarbage over a failing base: %v %v, want errBaseRead", got, err)
	}
}
