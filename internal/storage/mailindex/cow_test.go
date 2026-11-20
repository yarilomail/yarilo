package mailindex

import (
	"bytes"
	"encoding/binary"
	"math/rand/v2"
	"sync"
	"testing"
)

func cowRecord(l RecordLayout, uid uint32, flags MailFlag) []byte {
	raw := make([]byte, l.RecordSize)
	if err := EncodeRecord(raw, l, &Record{UID: uid, Flags: flags, Ext: map[string][]byte{}}); err != nil {
		panic(err)
	}
	return raw
}

func sameAsModel(t *testing.T, what string, r *Records, model [][]byte) {
	t.Helper()
	if r.Len() != len(model) {
		t.Fatalf("%s: %d records, model %d", what, r.Len(), len(model))
	}
	for seq, want := range model {
		if got := r.rec(seq); !bytes.Equal(got, want) {
			t.Fatalf("%s: record %d is %x, model %x", what, seq, got, want)
		}
	}
}

// The records change as a plain slice of records does, under any mix of
// changes and snapshots; a snapshot never sees a change made after it; and
// with no snapshot alive a change copies nothing (INDEX-MEMORY.md §4).
func TestRecordsChangeAsTheModelAndSnapshotsStayFrozen(t *testing.T) {
	l := chunkLayout(t)
	gid, _ := l.ExtID("guid")
	rng := rand.New(rand.NewPCG(1, 2))
	r, err := ChunksFromRecords(chunkRecs(l, 2*ChunkRecords+5), l)
	if err != nil {
		t.Fatal(err)
	}
	var model [][]byte
	for seq := range r.Len() {
		model = append(model, append([]byte(nil), r.rec(seq)...))
	}
	nextUID := r.UID(r.Len()-1) + 1
	type held struct {
		s     *Records
		model [][]byte
	}
	var snaps []held
	for step := 0; step < 5000; step++ {
		copies := r.ChunkCopies() + r.ListCopies()
		alive := len(snaps) > 0
		switch op := rng.IntN(10); {
		case op < 4 && len(model) > 0:
			seq := rng.IntN(len(model))
			f := MailFlag(rng.IntN(64))
			r.SetFlags(seq, f)
			model[seq] = append([]byte(nil), model[seq]...)
			model[seq][4] = byte(f)
		case op < 5 && len(model) > 0:
			seq := rng.IntN(len(model))
			g := make([]byte, 16)
			binary.LittleEndian.PutUint64(g, rng.Uint64())
			r.SetExt(seq, gid, g)
			e := l.Extensions[gid]
			model[seq] = append([]byte(nil), model[seq]...)
			copy(model[seq][e.RecordOffset:], g)
		case op < 7:
			raw := cowRecord(l, nextUID, FlagSeen)
			nextUID++
			r.Append(raw)
			model = append(model, raw)
		case op < 8 && len(model) > 0:
			seq := rng.IntN(len(model))
			r.Expunge(seq)
			model = append(model[:seq:seq], model[seq+1:]...)
		case op < 9:
			snaps = append(snaps, held{s: r.Snapshot(), model: append([][]byte(nil), model...)})
		case len(snaps) > 0:
			k := rng.IntN(len(snaps))
			sameAsModel(t, "a snapshot at release", snaps[k].s, snaps[k].model)
			snaps[k].s.Release()
			snaps = append(snaps[:k], snaps[k+1:]...)
		}
		if !alive && r.ChunkCopies()+r.ListCopies() != copies {
			t.Fatalf("step %d: a change with no snapshot alive copied", step)
		}
		if step%250 == 0 {
			sameAsModel(t, "the writer", r, model)
			for _, h := range snaps {
				sameAsModel(t, "a held snapshot", h.s, h.model)
			}
		}
	}
	sameAsModel(t, "the writer", r, model)
	for seq := 1; seq < r.Len(); seq++ {
		if s, ok := r.SeqOfUID(r.UID(seq)); !ok || s != seq {
			t.Fatalf("uid %d found at %d, %v", r.UID(seq), s, ok)
		}
	}
}

// Under one snapshot a change copies its own chunk and the list once; the
// 1000th change in that generation copies no more than the first.
func TestAChangeUnderASnapshotCopiesOneChunk(t *testing.T) {
	l := chunkLayout(t)
	r, err := ChunksFromRecords(chunkRecs(l, 4*ChunkRecords), l)
	if err != nil {
		t.Fatal(err)
	}
	s := r.Snapshot()
	r.SetFlags(5, FlagSeen)
	if r.ChunkCopies() != 1 || r.ListCopies() != 1 {
		t.Fatalf("first change copied %d chunks and %d lists, want 1 and 1", r.ChunkCopies(), r.ListCopies())
	}
	for i := 0; i < 999; i++ {
		r.SetFlags(6+i%100, FlagFlagged)
	}
	if r.ChunkCopies() != 1 || r.ListCopies() != 1 {
		t.Errorf("1000 changes in one chunk copied %d chunks and %d lists, want 1 and 1", r.ChunkCopies(), r.ListCopies())
	}
	if s.Flags(5) == FlagSeen {
		t.Error("the snapshot sees a change made after it")
	}
	s.Release()
	before := r.ChunkCopies()
	r.SetFlags(3*ChunkRecords, FlagSeen)
	if r.ChunkCopies() != before {
		t.Error("a change after the last snapshot was released copied its chunk")
	}
	if a := testing.AllocsPerRun(100, func() { r.SetFlags(7, FlagAnswered) }); a != 0 {
		t.Errorf("a change with no snapshot alive allocates %.0f times, want 0", a)
	}
}

// The publication protocol, as a caller keeps it: one lock for the writer's
// changes and the published snapshot; a reader takes the published one with
// a reference, the writer revokes it before writing in place. Readers never
// see a record half written. Run with -race.
func TestReadersNeverSeeAChangeHalfWritten(t *testing.T) {
	l := chunkLayout(t)
	gid, _ := l.ExtID("guid")
	r, err := ChunksFromRecords(chunkRecs(l, 2*ChunkRecords), l)
	if err != nil {
		t.Fatal(err)
	}
	// Every record's guid holds one value repeated; a torn write breaks it.
	fill := func(seq int, v byte) { r.SetExt(seq, gid, bytes.Repeat([]byte{v}, 16)) }
	for seq := range r.Len() {
		fill(seq, 0)
	}
	type pub struct {
		s       *Records
		refs    int
		revoked bool
	}
	var (
		mu        sync.Mutex
		published *pub
		fallbacks int
		reads     int
	)
	publish := func() { published = &pub{s: r.Snapshot()} }
	// revoke takes the published snapshot back before a change: released now
	// when nobody holds it, by its last reader otherwise. Under mu.
	revoke := func() {
		if published.refs == 0 {
			published.s.Release()
		} else {
			published.revoked = true
		}
		published = nil
	}
	mu.Lock()
	publish()
	mu.Unlock()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	torn := make(chan string, 1)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				mu.Lock()
				p := published
				reads++
				if p == nil {
					fallbacks++
					mu.Unlock()
					continue
				}
				p.refs++
				mu.Unlock()
				for seq := 0; seq < p.s.Len(); seq += 97 {
					g := p.s.Ext(seq, gid)
					if !bytes.Equal(g, bytes.Repeat(g[:1], 16)) {
						select {
						case torn <- "record half written":
						default:
						}
					}
				}
				mu.Lock()
				p.refs--
				if p.revoked && p.refs == 0 {
					p.s.Release()
				}
				mu.Unlock()
			}
		}()
	}
	for i := 0; i < 2000; i++ {
		mu.Lock()
		revoke()
		fill(i%r.Len(), byte(i))
		publish()
		mu.Unlock()
	}
	close(stop)
	wg.Wait()
	select {
	case msg := <-torn:
		t.Fatal(msg)
	default:
	}
	t.Logf("2000 changes: %d chunk copies, %d list copies; %d of %d reads found no published snapshot",
		r.ChunkCopies(), r.ListCopies(), fallbacks, reads)
}

// A fork changes its own copy and never the records it was forked from, which
// other readers keep reading; its own chunks it then changes in place.
func TestAForkNeverWritesTheRecordsItCameFrom(t *testing.T) {
	l := chunkLayout(t)
	base, err := ChunksFromRecords(chunkRecs(l, 3*ChunkRecords), l)
	if err != nil {
		t.Fatal(err)
	}
	want := base.Bytes()
	f := base.Fork()
	f.SetFlags(ChunkRecords+1, FlagFlagged)
	f.SetFlags(ChunkRecords+2, FlagFlagged)
	f.Append(cowRecord(l, 1<<20, FlagSeen))
	f.Expunge(0)
	if !bytes.Equal(base.Bytes(), want) {
		t.Fatal("a change to the fork reached the records it came from")
	}
	if f.ChunkCopies() != 2 {
		t.Errorf("the fork copied %d chunks, want 2: chunk 1 it changed twice and chunk 0 it expunged from; the append starts a chunk of its own", f.ChunkCopies())
	}
	if f.Flags(ChunkRecords) != FlagFlagged || f.Len() != 3*ChunkRecords {
		t.Errorf("the fork reads flags %d at its record %d and holds %d", f.Flags(ChunkRecords), ChunkRecords, f.Len())
	}
	before := f.ChunkCopies()
	f.SetFlags(ChunkRecords, FlagAnswered)
	if f.ChunkCopies() != before {
		t.Error("the fork copied a chunk it already owns")
	}
}
