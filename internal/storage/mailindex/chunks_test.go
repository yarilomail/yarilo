package mailindex

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
)

func chunkLayout(t *testing.T) RecordLayout {
	t.Helper()
	l, err := ComputeRecordLayout([]Extension{
		{Name: "modseq", RecordSize: 8, RecordAlign: 8},
		{Name: "guid", RecordSize: 16, RecordAlign: 4},
		{Name: "vsize", RecordSize: 4, RecordAlign: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func chunkRecs(l RecordLayout, n int) []*Record {
	out := make([]*Record, n)
	for i := range out {
		g := make([]byte, 16)
		binary.LittleEndian.PutUint32(g, uint32(i))
		ms := make([]byte, 8)
		binary.LittleEndian.PutUint64(ms, uint64(1000+i))
		out[i] = &Record{UID: uint32(2*i + 1), Flags: MailFlag(i % 32), Ext: map[string][]byte{"guid": g, "modseq": ms}}
	}
	return out
}

// The chunked form reads every field as the map form does, across chunk
// boundaries, and gives back the bytes it was built from.
func TestChunksReadAsTheRecordsTheyHold(t *testing.T) {
	l := chunkLayout(t)
	for _, n := range []int{0, 1, ChunkRecords - 1, ChunkRecords, ChunkRecords + 1, 3*ChunkRecords + 17} {
		recs := chunkRecs(l, n)
		r, err := ChunksFromRecords(recs, l)
		if err != nil {
			t.Fatal(err)
		}
		if r.Len() != n || r.Chunks() != (n+ChunkRecords-1)/ChunkRecords {
			t.Fatalf("n=%d: %d records in %d chunks", n, r.Len(), r.Chunks())
		}
		gid, _ := l.ExtID("guid")
		for seq, want := range recs {
			got, err := r.Record(seq)
			if err != nil {
				t.Fatal(err)
			}
			if r.UID(seq) != want.UID || r.Flags(seq) != want.Flags || !bytes.Equal(r.Ext(seq, gid), want.Ext["guid"]) {
				t.Fatalf("n=%d seq %d: uid %d flags %d guid %x, want %d %d %x", n, seq, r.UID(seq), r.Flags(seq), r.Ext(seq, gid), want.UID, want.Flags, want.Ext["guid"])
			}
			if !reflect.DeepEqual(got.Ext["modseq"], want.Ext["modseq"]) {
				t.Fatalf("n=%d seq %d: modseq %x, want %x", n, seq, got.Ext["modseq"], want.Ext["modseq"])
			}
		}
		raw := make([]byte, n*int(l.RecordSize))
		for i, rec := range recs {
			if err := EncodeRecord(raw[i*int(l.RecordSize):(i+1)*int(l.RecordSize)], l, rec); err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.Equal(r.Bytes(), raw) {
			t.Fatalf("n=%d: Bytes differs from the encoded records", n)
		}
	}
}

// A uid is found by binary search; a uid between two is not.
func TestChunksFindAUIDBySearch(t *testing.T) {
	l := chunkLayout(t)
	r, err := ChunksFromRecords(chunkRecs(l, 2*ChunkRecords+5), l)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		uid  uint32
		seq  int
		find bool
	}{{1, 0, true}, {2, 1, false}, {2*ChunkRecords + 1, ChunkRecords, true}, {9999999, r.Len(), false}} {
		seq, ok := r.SeqOfUID(tc.uid)
		if seq != tc.seq || ok != tc.find {
			t.Errorf("uid %d: seq %d found %v, want %d %v", tc.uid, seq, ok, tc.seq, tc.find)
		}
	}
}

// A records section that is not whole records is refused, as Read refuses it.
func TestChunksRefuseAPartialRecord(t *testing.T) {
	l := chunkLayout(t)
	if _, err := ChunksFromBytes(make([]byte, int(l.RecordSize)+1), l); err == nil {
		t.Error("a trailing partial record was taken")
	}
	if _, ok := l.ExtID("nope"); ok {
		t.Error("an extension the layout does not carry resolved")
	}
}

// Building chunks allocates per chunk, not per record: the point of the form.
func TestChunksAllocatePerChunk(t *testing.T) {
	l := chunkLayout(t)
	raw, err := ChunksFromRecords(chunkRecs(l, 10*ChunkRecords), l)
	if err != nil {
		t.Fatal(err)
	}
	b := raw.Bytes()
	allocs := testing.AllocsPerRun(5, func() {
		if _, err := ChunksFromBytes(b, l); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 2*10+4 {
		t.Errorf("%v allocations for 10 chunks of %d records, want about one a chunk", allocs, ChunkRecords)
	}
}
