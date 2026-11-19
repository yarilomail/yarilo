package mailindex

import (
	"encoding/binary"
	"fmt"
	"sort"
	"sync/atomic"
)

// ChunkRecords is how many records one chunk holds: the unit a change copies
// and a lazy read loads (INDEX-MEMORY.md §2).
const ChunkRecords = 1024

// ExtID names a record extension by its place in a layout, resolved once, so
// a read a message is an offset, not a map lookup.
type ExtID int

// ExtID resolves name in the layout; ok is false for an extension the layout
// does not carry in its records.
func (l RecordLayout) ExtID(name string) (ExtID, bool) {
	for i, e := range l.Extensions {
		if e.Name == name && e.RecordSize != 0 {
			return ExtID(i), true
		}
	}
	return -1, false
}

// Chunk is up to ChunkRecords records exactly as they lie on disk. A chunk is
// never written once built: a change makes a new one.
type Chunk struct {
	raw  []byte
	size uint32
	n    int
}

// Len is the records the chunk holds.
func (c *Chunk) Len() int { return c.n }

func (c *Chunk) at(i int) []byte {
	off := uint32(i) * c.size
	return c.raw[off : off+c.size]
}

// Records is a folder's records in chunks, with their layout. A lazy one
// (OpenLazy) loads its chunks from the base on Load.
type Records struct {
	layout RecordLayout
	chunks []atomicChunk
	n      int
	src    *lazySource
}

// Layout is the records' layout.
func (r *Records) Layout() RecordLayout { return r.layout }

// Len is the number of records.
func (r *Records) Len() int { return r.n }

// Chunks is the number of chunks.
func (r *Records) Chunks() int { return len(r.chunks) }

// Loaded reports whether chunk ci is in memory.
func (r *Records) Loaded(ci int) bool { return r.chunks[ci].Load() != nil }

func (r *Records) rec(seq int) []byte {
	c := r.chunks[seq/ChunkRecords].Load()
	if c == nil {
		panic(fmt.Sprintf("mailindex: record %d read before its chunk was loaded", seq))
	}
	return c.at(seq % ChunkRecords)
}

// UID is the uid of the record at seq, from 0.
func (r *Records) UID(seq int) uint32 { return binary.LittleEndian.Uint32(r.rec(seq)[0:]) }

// Flags is the flags byte of the record at seq.
func (r *Records) Flags(seq int) MailFlag { return MailFlag(r.rec(seq)[4]) }

// Ext is the extension's bytes in the record at seq: a view into the chunk, to
// be copied by a caller that keeps it past the Records it came from.
func (r *Records) Ext(seq int, id ExtID) []byte {
	e := r.layout.Extensions[id]
	return r.rec(seq)[e.RecordOffset : e.RecordOffset+e.RecordSize]
}

// SeqOfUID finds a uid by binary search: records are in uid order on disk.
func (r *Records) SeqOfUID(uid uint32) (int, bool) {
	seq := sort.Search(r.n, func(i int) bool { return r.UID(i) >= uid })
	return seq, seq < r.n && r.UID(seq) == uid
}

// Record decodes the record at seq into the map form, for the callers that
// still take it.
func (r *Records) Record(seq int) (Record, error) {
	return DecodeRecord(r.rec(seq), r.layout)
}

// ChunksFromBytes splits a records section, as read from disk, into chunks
// without decoding a record. raw must be a whole number of records; it is
// copied, so the caller's buffer can be reused.
func ChunksFromBytes(raw []byte, layout RecordLayout) (*Records, error) {
	size := layout.RecordSize
	if size == 0 {
		return nil, fmt.Errorf("mailindex/chunks: zero record size: %w", ErrCorrupted)
	}
	if uint32(len(raw))%size != 0 {
		return nil, fmt.Errorf("mailindex/chunks: %d bytes is not a whole number of %d-byte records: %w", len(raw), size, ErrShortRead)
	}
	n := len(raw) / int(size)
	out := &Records{layout: layout, n: n, chunks: make([]atomicChunk, chunkCount(n))}
	for ci := range out.chunks {
		start, end := ci*ChunkRecords, min((ci+1)*ChunkRecords, n)
		b := make([]byte, (end-start)*int(size))
		copy(b, raw[start*int(size):end*int(size)])
		out.chunks[ci].Store(&Chunk{raw: b, size: size, n: end - start})
	}
	return out, nil
}

// ChunksFromRecords encodes the map form into chunks.
func ChunksFromRecords(recs []*Record, layout RecordLayout) (*Records, error) {
	raw := make([]byte, len(recs)*int(layout.RecordSize))
	for i, rec := range recs {
		off := i * int(layout.RecordSize)
		if err := EncodeRecord(raw[off:off+int(layout.RecordSize)], layout, rec); err != nil {
			return nil, err
		}
	}
	return ChunksFromBytes(raw, layout)
}

// Bytes is the records section as it lies on disk; every chunk is loaded.
func (r *Records) Bytes() []byte {
	out := make([]byte, 0, r.n*int(r.layout.RecordSize))
	for ci := range r.chunks {
		c := r.chunks[ci].Load()
		out = append(out, c.raw[:c.n*int(c.size)]...)
	}
	return out
}

func chunkCount(n int) int { return (n + ChunkRecords - 1) / ChunkRecords }

type atomicChunk = atomic.Pointer[Chunk]
