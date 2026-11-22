package mdboxmap

import (
	"testing"
)

// The record area is the lookup structure, so the search has to be right at its
// edges: an off-by-one at either end silently reports a live message missing,
// and a missing message is one purge is free to reclaim.
func TestLookupAtTheEdgesOfTheRecordArea(t *testing.T) {
	dir := t.TempDir()
	seedV1(t, dir, v1Entries(), 8, 2, 0) // map_uids 1, 2, 7

	m, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer m.Close() //nolint:errcheck

	tests := []struct {
		name string
		uid  uint32
		want bool
	}{
		{"first record", 1, true},
		{"middle record", 2, true},
		{"last record", 7, true},
		{"below the first", 0, false},
		{"between two records", 3, false},
		{"above the last", 9, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, ok, err := m.Lookup(tc.uid)
			if err != nil {
				t.Fatalf("Lookup(%d): %v", tc.uid, err)
			}
			if ok != tc.want {
				t.Fatalf("Lookup(%d) found=%v, want %v", tc.uid, ok, tc.want)
			}
			if ok && e.UID != tc.uid {
				t.Errorf("Lookup(%d) returned the record of map_uid %d", tc.uid, e.UID)
			}
		})
	}
}

// The layer shadows the base by map_uid: a walk that merged them by position
// would emit a changed record twice or a removed one at all.
func TestTheLayerShadowsTheBaseInAWalk(t *testing.T) {
	s := store{base: memBase([]MapEntry{{UID: 1}, {UID: 3, Size: 30}, {UID: 5}}, 6), count: 3}
	for _, e := range []MapEntry{{UID: 3, Size: 31}, {UID: 4}, {UID: 9}} {
		if err := s.put(e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.remove(5); err != nil {
		t.Fatal(err)
	}
	var got []MapEntry
	sn := s.snapshot()
	defer sn.release()
	if err := sn.each(func(e MapEntry) error { got = append(got, e); return nil }); err != nil {
		t.Fatal(err)
	}
	want := []MapEntry{{UID: 1}, {UID: 3, Size: 31}, {UID: 4}, {UID: 9}}
	if len(got) != len(want) || s.count != len(want) {
		t.Fatalf("walk %+v, count %d, want %+v", got, s.count, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record %d is %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A header that round-trips is the precondition for everything above: the
// record count decides where the record area ends, and the log pairing decides
// how much of the log is replayed on top of it.
func TestBaseHeaderRoundTrips(t *testing.T) {
	want := baseHeader{
		Version:       baseVersion2,
		RecordSize:    baseRecordLen,
		RecordCount:   3,
		NextMapUID:    8,
		HighestFileID: 2,
		RebuildCount:  5,
		CreateFileID:  2,
		CreateTime:    1723200000,
		FoldedOffset:  4096,
		FoldedLineage: 6,
		Lineage:       7,
		RecordsDigest: 0x1234_5678_9abc_def0,
		IndexID:       4242,
	}
	got, err := decodeBaseHeader(encodeBaseHeader(want))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != want {
		t.Errorf("header round-tripped to %+v, want %+v", got, want)
	}
}

// A record that round-trips at a non-zero index proves the offset arithmetic,
// not just the codec.
func TestRecordRoundTripsAtAnOffset(t *testing.T) {
	want := MapEntry{UID: 9, FileID: 3, Offset: 1024, Size: 777, RefCount: 2, GUID: [16]byte{0xde, 0xad}}
	b := memBase([]MapEntry{{UID: 1}, want}, 10)
	got, ok, err := b.find(want.UID)
	if err != nil || !ok {
		t.Fatalf("record not found: %v", err)
	}
	if got != want {
		t.Errorf("record round-tripped to %+v, want %+v", got, want)
	}
}
