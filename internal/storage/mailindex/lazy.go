package mailindex

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
)

// ErrBaseMoved says a lazy load found the base it was opened on gone or
// replaced: the caller opens the current base and reads again.
var ErrBaseMoved = errors.New("mailindex: the base was replaced under a lazy read")

// ChunkReadSeam is a test seam wrapping the reader a lazy load reads through.
// Nil everywhere else.
var ChunkReadSeam func(path string, r io.ReaderAt) io.ReaderAt

var lazyOpens atomic.Int64

// LazyOpens counts the opens of a base that lazy loads made, for a test or a
// measurement to say what a command costs in files.
func LazyOpens() int64 { return lazyOpens.Load() }

// lazySource is what a lazy load reopens and checks: no descriptor is kept
// between loads.
type lazySource struct {
	path    string
	ident   os.FileInfo
	header  []byte
	dataOff int64
}

// OpenLazy reads the header and the extension table of the base at path and
// leaves its records on disk: the File has no Records, the *Records loads
// them by chunk.
func OpenLazy(path string) (*File, *Records, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("mailindex/lazy: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("mailindex/lazy: %w", err)
	}
	pre := make([]byte, HeaderMinSize)
	if _, err := io.ReadFull(f, pre); err != nil {
		return nil, nil, fmt.Errorf("mailindex/lazy: header truncated: %w", ErrShortRead)
	}
	hdr, err := DecodeHeaderBytes(pre)
	if err != nil {
		return nil, nil, fmt.Errorf("mailindex/lazy: %w", err)
	}
	if hdr.HeaderSize < uint32(hdr.BaseHeaderSize) || int64(hdr.HeaderSize) > st.Size() {
		return nil, nil, fmt.Errorf("mailindex/lazy: HeaderSize=%d: %w", hdr.HeaderSize, ErrCorrupted)
	}
	head := make([]byte, hdr.HeaderSize)
	if _, err := f.ReadAt(head, 0); err != nil {
		return nil, nil, fmt.Errorf("mailindex/lazy: header: %w", err)
	}
	parsed, err := Read(bytes.NewReader(head))
	if err != nil {
		return nil, nil, err
	}
	size := int64(parsed.Layout.RecordSize)
	body := st.Size() - int64(hdr.HeaderSize)
	if body%size != 0 {
		return nil, nil, fmt.Errorf("mailindex/lazy: trailing partial record: %w", ErrShortRead)
	}
	n := int(body / size)
	recs := &Records{
		layout: parsed.Layout,
		n:      n,
		chunks: make([]atomicChunk, chunkCount(n)),
		src:    &lazySource{path: path, ident: st, header: head, dataOff: int64(hdr.HeaderSize)},
	}
	return parsed, recs, nil
}

// Load brings into memory every chunk holding a record in [lo, hi), through
// one open of the base. A base that is gone, another file, or a header that
// changed since OpenLazy is ErrBaseMoved, and so is any read that fails.
func (r *Records) Load(lo, hi int) error {
	if r.src == nil || lo >= hi {
		return nil
	}
	first, last := lo/ChunkRecords, (hi-1)/ChunkRecords
	missing := false
	for ci := first; ci <= last; ci++ {
		if r.chunks[ci].Load() == nil {
			missing = true
			break
		}
	}
	if !missing {
		return nil
	}
	lazyOpens.Add(1)
	f, err := os.Open(r.src.path)
	if err != nil {
		return fmt.Errorf("mailindex/lazy: %w: %w", ErrBaseMoved, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !os.SameFile(st, r.src.ident) {
		return fmt.Errorf("mailindex/lazy: %w: another file at %s", ErrBaseMoved, r.src.path)
	}
	var rd io.ReaderAt = f
	if ChunkReadSeam != nil {
		rd = ChunkReadSeam(r.src.path, rd)
	}
	head := make([]byte, len(r.src.header))
	if err := readFull(rd, head, 0); err != nil {
		return err
	}
	if !bytes.Equal(head, r.src.header) {
		return fmt.Errorf("mailindex/lazy: %w: the header changed", ErrBaseMoved)
	}
	size := int(r.layout.RecordSize)
	for ci := first; ci <= last; ci++ {
		if r.chunks[ci].Load() != nil {
			continue
		}
		cn := min(ChunkRecords, r.n-ci*ChunkRecords)
		raw := make([]byte, cn*size)
		if err := readFull(rd, raw, r.src.dataOff+int64(ci*ChunkRecords*size)); err != nil {
			return err
		}
		r.chunks[ci].CompareAndSwap(nil, &Chunk{raw: raw, size: uint32(size), n: cn})
	}
	return nil
}

// LoadAll loads every chunk.
func (r *Records) LoadAll() error { return r.Load(0, r.n) }

func readFull(rd io.ReaderAt, p []byte, off int64) error {
	n, err := rd.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = ErrShortRead
	}
	return fmt.Errorf("mailindex/lazy: %w: %w", ErrBaseMoved, err)
}

// Decode loads every chunk and decodes it into the map form.
func (r *Records) Decode() ([]*Record, error) {
	if err := r.LoadAll(); err != nil {
		return nil, err
	}
	out := make([]*Record, r.n)
	for i := range out {
		rec, err := r.Record(i)
		if err != nil {
			return nil, err
		}
		out[i] = &rec
	}
	return out, nil
}
