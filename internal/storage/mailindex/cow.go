package mailindex

import (
	"fmt"
	"sync/atomic"
)

// writer is the write side of a Records: which chunks, and whether the chunk
// list, are its own. A change writes in place what is its own and copies
// what a live snapshot still shares (INDEX-MEMORY.md §4).
type writer struct {
	ownList bool
	own     []bool
	shared  bool // a snapshot was taken since the writer last owned everything
	live    *atomic.Int32
	// copies counts chunks and lists copied, for a test to pin the cost.
	chunkCopies, listCopies int
}

func newWriter(chunks int) *writer {
	own := make([]bool, chunks)
	for i := range own {
		own[i] = true
	}
	return &writer{ownList: true, own: own, live: new(atomic.Int32)}
}

// Snapshot is a read-only view of the records as they are now. It shares
// every chunk with r until r changes one; Release says the reader is done.
func (r *Records) Snapshot() *Records {
	w := r.mustWriter()
	w.live.Add(1)
	w.ownList = false
	w.shared = true
	clear(w.own)
	return &Records{layout: r.layout, chunks: r.chunks, n: r.n, src: r.src, starts: r.starts, w: &writer{live: w.live}}
}

// Release ends a snapshot's hold: with none left, the writer owns every chunk
// again and changes them in place.
func (r *Records) Release() {
	if r.w == nil || r.w.own != nil {
		panic("mailindex: Release of a records set that is not a snapshot")
	}
	if r.w.live.Add(-1) < 0 {
		panic("mailindex: snapshot released twice")
	}
	r.w = nil
}

// ChunkCopies and ListCopies are what the writer's changes copied so far.
func (r *Records) ChunkCopies() int { return r.mustWriter().chunkCopies }

// ListCopies is how many times the chunk list was copied.
func (r *Records) ListCopies() int { return r.mustWriter().listCopies }

func (r *Records) mustWriter() *writer {
	if r.w == nil || r.w.own == nil {
		panic("mailindex: a snapshot is read-only")
	}
	return r.w
}

// owned makes chunk ci, and the list holding it, the writer's own.
func (r *Records) owned(ci int) *Chunk {
	w := r.ownList()
	c := r.chunks[ci].Load()
	if c == nil {
		panic(fmt.Sprintf("mailindex: chunk %d written before it was loaded", ci))
	}
	if !w.own[ci] {
		raw := make([]byte, len(c.raw), ChunkRecords*int(c.size))
		copy(raw, c.raw)
		c = &Chunk{raw: raw, size: c.size, n: c.n}
		r.chunks[ci].Store(c)
		w.own[ci] = true
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
		r.chunks[last+1].Store(&Chunk{raw: make([]byte, 0, ChunkRecords*size), size: uint32(size)})
		w.own = append(w.own, true)
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

// ownList makes the chunk list the writer's own: with no snapshot left it
// already is, along with every chunk; under one it is copied.
func (r *Records) ownList() *writer {
	w := r.mustWriter()
	if w.shared && w.live.Load() == 0 {
		w.ownList, w.shared = true, false
		for i := range w.own {
			w.own[i] = true
		}
	}
	if w.ownList {
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
	w.ownList = true
	w.listCopies++
	return w
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
		r.w.own = append(r.w.own[:ci], r.w.own[ci+1:]...)
	}
	r.n--
}
