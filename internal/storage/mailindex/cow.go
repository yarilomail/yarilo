package mailindex

import (
	"fmt"
	"sync/atomic"
)

// writer is the write side of a Records. A change writes in place a chunk it
// wrote in this generation, or any chunk of its own once no snapshot is
// alive; anything else it copies first (INDEX-MEMORY.md §4).
type writer struct {
	gen     uint64 // bumped by each Snapshot
	listGen uint64
	ownList bool // the chunk list was made by this writer, in listGen
	live    *atomic.Int32
	snap    bool // a snapshot's stub: it releases, never writes
	// copies counts chunks and lists copied, for a test to pin the cost.
	chunkCopies, listCopies int
}

func newWriter() *writer { return &writer{ownList: true, live: new(atomic.Int32)} }

// Snapshot is a read-only view of the records as they are now. It shares
// every chunk with r until r changes one; Release says the reader is done.
func (r *Records) Snapshot() *Records {
	w := r.mustWriter()
	w.live.Add(1)
	w.gen++
	return &Records{layout: r.layout, chunks: r.chunks, n: r.n, src: r.src, starts: r.starts, w: &writer{live: w.live, snap: true}}
}

// Fork is a writer of its own over r's records as they are now: it copies a
// chunk of r's before its first change to it, and r is not to change after.
func (r *Records) Fork() *Records {
	return &Records{layout: r.layout, chunks: r.chunks, n: r.n, src: r.src, starts: r.starts, w: &writer{live: new(atomic.Int32)}}
}

// Release ends a snapshot's hold: with none left, the writer changes its own
// chunks in place again.
func (r *Records) Release() {
	if r.w == nil || !r.w.snap {
		panic("mailindex: Release of a records set that is not a snapshot")
	}
	if r.w.live.Add(-1) < 0 {
		panic("mailindex: snapshot released twice")
	}
	r.w = nil
}

// ChunkCopies is how many chunks the writer's changes copied so far.
func (r *Records) ChunkCopies() int { return r.mustWriter().chunkCopies }

// ListCopies is how many times the chunk list was copied.
func (r *Records) ListCopies() int { return r.mustWriter().listCopies }

func (r *Records) mustWriter() *writer {
	if r.w == nil || r.w.snap {
		panic("mailindex: a snapshot is read-only")
	}
	return r.w
}

func (w *writer) writable(c *Chunk) bool {
	return c.owner == w && (c.gen == w.gen || w.live.Load() == 0)
}

// ownList makes the chunk list the writer's own, copying it while a snapshot
// or the records forked from still use it.
func (r *Records) ownList() *writer {
	w := r.mustWriter()
	if w.ownList && (w.listGen == w.gen || w.live.Load() == 0) {
		return w
	}
	list := make([]atomicChunk, len(r.chunks), len(r.chunks)+1)
	for i := range r.chunks {
		list[i].Store(r.chunks[i].Load())
	}
	r.chunks = list
	if r.starts != nil {
		r.starts = append([]int(nil), r.starts...)
	}
	w.ownList, w.listGen = true, w.gen
	w.listCopies++
	return w
}

// owned makes chunk ci, and the list holding it, the writer's own.
func (r *Records) owned(ci int) *Chunk {
	w := r.ownList()
	c := r.chunks[ci].Load()
	if c == nil {
		panic(fmt.Sprintf("mailindex: chunk %d written before it was loaded", ci))
	}
	if !w.writable(c) {
		raw := make([]byte, len(c.raw), ChunkRecords*int(c.size))
		copy(raw, c.raw)
		c = &Chunk{raw: raw, size: c.size, n: c.n, owner: w, gen: w.gen}
		r.chunks[ci].Store(c)
		w.chunkCopies++
	}
	return c
}

// SetFlags writes the flags byte of the record at seq.
func (r *Records) SetFlags(seq int, f MailFlag) {
	ci, i := r.locate(seq)
	r.owned(ci).at(i)[4] = byte(f)
}

// SetExt writes the extension's bytes of the record at seq.
func (r *Records) SetExt(seq int, id ExtID, b []byte) {
	e := r.layout.Extensions[id]
	if len(b) != int(e.RecordSize) {
		panic(fmt.Sprintf("mailindex: %s is %d bytes, not %d", e.Name, len(b), e.RecordSize))
	}
	ci, i := r.locate(seq)
	copy(r.owned(ci).at(i)[e.RecordOffset:], b)
}

// Append adds one record, encoded in the layout, after the last.
func (r *Records) Append(raw []byte) {
	size := int(r.layout.RecordSize)
	if len(raw) != size {
		panic(fmt.Sprintf("mailindex: a record is %d bytes, not %d", len(raw), size))
	}
	last := len(r.chunks) - 1
	if last < 0 || r.chunks[last].Load().n == ChunkRecords {
		w := r.ownList()
		r.chunks = append(r.chunks, atomicChunk{})
		r.chunks[last+1].Store(&Chunk{raw: make([]byte, 0, ChunkRecords*size), size: uint32(size), owner: w, gen: w.gen})
		if r.starts != nil {
			r.starts = append(r.starts, r.n)
		}
		last++
	}
	c := r.owned(last)
	c.raw = append(c.raw, raw...)
	c.n++
	r.n++
}

// Expunge removes the record at seq, moving the rest of its chunk down; a
// chunk left empty leaves the list.
func (r *Records) Expunge(seq int) {
	ci, i := r.locate(seq)
	c := r.owned(ci)
	size := int(c.size)
	copy(c.raw[i*size:], c.raw[(i+1)*size:])
	c.raw = c.raw[:len(c.raw)-size]
	c.n--
	if r.starts == nil {
		r.starts = make([]int, len(r.chunks))
		for k := range r.starts {
			r.starts[k] = k * ChunkRecords
		}
	}
	for k := ci + 1; k < len(r.starts); k++ {
		r.starts[k]--
	}
	if c.n == 0 {
		for k := ci; k+1 < len(r.chunks); k++ {
			r.chunks[k].Store(r.chunks[k+1].Load())
		}
		r.chunks = r.chunks[:len(r.chunks)-1]
		r.starts = append(r.starts[:ci], r.starts[ci+1:]...)
	}
	r.n--
}

// RecordBytes is a copy of the record at seq as it lies on disk.
func (r *Records) RecordBytes(seq int) []byte { return append([]byte(nil), r.rec(seq)...) }

// SetRecord writes the record at seq whole, from bytes in the layout.
func (r *Records) SetRecord(seq int, raw []byte) {
	ci, i := r.locate(seq)
	copy(r.owned(ci).at(i), raw)
}

// Insert puts a record, encoded in the layout, at seq, moving the rest of its
// chunk up; a full chunk is split in two. For an append below the last uid,
// which the uid order the records keep would not survive otherwise.
func (r *Records) Insert(seq int, raw []byte) {
	if seq >= r.n {
		r.Append(raw)
		return
	}
	size := int(r.layout.RecordSize)
	if len(raw) != size {
		panic(fmt.Sprintf("mailindex: a record is %d bytes, not %d", len(raw), size))
	}
	ci, i := r.locate(seq)
	c := r.owned(ci)
	w := r.w
	if r.starts == nil {
		r.starts = make([]int, len(r.chunks))
		for k := range r.starts {
			r.starts[k] = k * ChunkRecords
		}
	}
	if c.n == ChunkRecords {
		half := c.n / 2
		tail := &Chunk{raw: make([]byte, (c.n-half)*size, ChunkRecords*size), size: c.size, n: c.n - half, owner: w, gen: w.gen}
		copy(tail.raw, c.raw[half*size:])
		c.raw, c.n = c.raw[:half*size], half
		r.chunks = append(r.chunks, atomicChunk{})
		for k := len(r.chunks) - 1; k > ci+1; k-- {
			r.chunks[k].Store(r.chunks[k-1].Load())
		}
		r.chunks[ci+1].Store(tail)
		r.starts = append(r.starts[:ci+1], append([]int{r.starts[ci] + half}, r.starts[ci+1:]...)...)
		if i > half {
			ci, i, c = ci+1, i-half, tail
		}
	}
	c.raw = append(c.raw, raw...) // room for one more
	copy(c.raw[(i+1)*size:], c.raw[i*size:len(c.raw)-size])
	copy(c.raw[i*size:], raw)
	c.n++
	for k := ci + 1; k < len(r.starts); k++ {
		r.starts[k]++
	}
	r.n++
}
