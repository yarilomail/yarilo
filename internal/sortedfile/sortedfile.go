// Package sortedfile is a file of sorted key-value sections, searched by
// key with a few positioned reads and never loaded whole: the base under a
// per-user log, as the thread store and the GUID store use it
// (docs-internal THREADS-STORE.md).
//
// Layout, little endian:
//
//	header   magic "YSF1", version u32, generation u64, absorbed u64,
//	         sections u32, then per section: id u32, count u64,
//	         entries offset u64, strings offset u64, strings length u64
//	entries  count x 24 bytes: hash u64, key offset u32, key length u16,
//	         value offset u32, value length u16, pad u32
//	strings  the keys and values the entries point into
//
// Entries are sorted by hash, then by key. The hash is the first 8 bytes of
// SHA-256: a sender chooses the keys, so a long run of equal hashes is not
// theirs to make (reviewer, 2026-11-20). Equal hashes are told apart by key.
package sortedfile

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
)

const (
	magic     = "YSF1"
	version   = 1
	entrySize = 24
	// headFixed is the header before the section table.
	headFixed  = 4 + 4 + 8 + 8 + 4
	sectionLen = 4 + 8 + 8 + 8 + 8
)

// ErrMoved says the file was replaced or cut under a reader: the caller
// opens the pair again and asks once more (INDEX-MEMORY.md §4).
var ErrMoved = errors.New("sortedfile: the file was replaced under a read")

// Hash is the key's place in the order.
func Hash(key []byte) uint64 {
	sum := sha256.Sum256(key)
	return binary.BigEndian.Uint64(sum[:8])
}

// hashSeam is a test seam: a hash that collides on purpose. Nil elsewhere.
var hashSeam func([]byte) uint64

func hashOf(key []byte) uint64 {
	if hashSeam != nil {
		return hashSeam(key)
	}
	return Hash(key)
}

// Less is the file's order: by hash, then by key.
func Less(a, b []byte) bool {
	ha, hb := hashOf(a), hashOf(b)
	if ha != hb {
		return ha < hb
	}
	return bytes.Compare(a, b) < 0
}

type section struct {
	id         uint32
	count      uint64
	entries    uint64
	strings    uint64
	stringsLen uint64
}

// Reader answers lookups from an open file, through the descriptor it
// opened: a file renamed over it is not read, and one cut or gone under it on
// NFS fails the read, which is ErrMoved.
type Reader struct {
	f          *os.File
	path       string
	Generation uint64
	// Absorbed is how far into the log of the previous generation this file
	// already holds: a reader with that log applies only what lies past it.
	Absorbed uint64
	sections map[uint32]section
}

// readAtSeam is a test seam wrapping every positioned read. Nil elsewhere.
var readAtSeam func(path string, r io.ReaderAt) io.ReaderAt

// SetReadSeam installs readAtSeam for a test in another package: a base on
// NFS that fails or cuts its reads. Nil removes it.
func SetReadSeam(f func(path string, r io.ReaderAt) io.ReaderAt) { readAtSeam = f }

// Open reads the header and the section table.
func Open(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("sortedfile: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close() //nolint:errcheck
		return nil, fmt.Errorf("sortedfile: %w: %w", ErrMoved, err)
	}
	r := &Reader{f: f, path: path, sections: map[uint32]section{}}
	head := make([]byte, headFixed)
	if err := r.readFull(head, 0); err != nil {
		f.Close() //nolint:errcheck
		return nil, err
	}
	if string(head[0:4]) != magic || binary.LittleEndian.Uint32(head[4:]) != version {
		f.Close() //nolint:errcheck
		return nil, fmt.Errorf("sortedfile: %s is not a version %d file", path, version)
	}
	r.Generation = binary.LittleEndian.Uint64(head[8:])
	r.Absorbed = binary.LittleEndian.Uint64(head[16:])
	n := binary.LittleEndian.Uint32(head[24:])
	table := make([]byte, int(n)*sectionLen)
	if err := r.readFull(table, headFixed); err != nil {
		f.Close() //nolint:errcheck
		return nil, err
	}
	for i := 0; i < int(n); i++ {
		b := table[i*sectionLen:]
		s := section{
			id:         binary.LittleEndian.Uint32(b[0:]),
			count:      binary.LittleEndian.Uint64(b[4:]),
			entries:    binary.LittleEndian.Uint64(b[12:]),
			strings:    binary.LittleEndian.Uint64(b[20:]),
			stringsLen: binary.LittleEndian.Uint64(b[28:]),
		}
		if s.entries+s.count*entrySize > uint64(st.Size()) || s.strings+s.stringsLen > uint64(st.Size()) {
			f.Close() //nolint:errcheck
			return nil, fmt.Errorf("sortedfile: %w: section %d past the end of %s", ErrMoved, s.id, path)
		}
		r.sections[s.id] = s
	}
	return r, nil
}

// Close lets the descriptor go.
func (r *Reader) Close() error { return r.f.Close() }

// Count is how many entries a section holds.
func (r *Reader) Count(id uint32) uint64 { return r.sections[id].count }

// readFull reads exactly len(p) at off; a short or failed read, ESTALE
// among them, is the file having moved.
func (r *Reader) readFull(p []byte, off int64) error {
	var rd io.ReaderAt = r.f
	if readAtSeam != nil {
		rd = readAtSeam(r.path, rd)
	}
	n, err := rd.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return fmt.Errorf("sortedfile: %w: %w", ErrMoved, err)
}

type entry struct {
	hash           uint64
	keyOff, valOff uint32
	keyLen, valLen uint16
}

func (r *Reader) entryAt(s section, i uint64) (entry, error) {
	b := make([]byte, entrySize)
	if err := r.readFull(b, int64(s.entries+i*entrySize)); err != nil {
		return entry{}, err
	}
	return entry{
		hash:   binary.LittleEndian.Uint64(b[0:]),
		keyOff: binary.LittleEndian.Uint32(b[8:]),
		keyLen: binary.LittleEndian.Uint16(b[12:]),
		valOff: binary.LittleEndian.Uint32(b[14:]),
		valLen: binary.LittleEndian.Uint16(b[18:]),
	}, nil
}

func (r *Reader) str(s section, off uint32, n uint16) ([]byte, error) {
	if uint64(off)+uint64(n) > s.stringsLen {
		return nil, fmt.Errorf("sortedfile: %w: a string past its section in %s", ErrMoved, r.path)
	}
	b := make([]byte, n)
	if err := r.readFull(b, int64(s.strings)+int64(off)); err != nil {
		return nil, err
	}
	return b, nil
}

// Get is the value under key in section id.
func (r *Reader) Get(id uint32, key []byte) ([]byte, bool, error) {
	s, ok := r.sections[id]
	if !ok || s.count == 0 {
		return nil, false, nil
	}
	h := hashOf(key)
	var lerr error
	first := sort.Search(int(s.count), func(i int) bool {
		if lerr != nil {
			return true
		}
		e, err := r.entryAt(s, uint64(i))
		if err != nil {
			lerr = err
			return true
		}
		return e.hash >= h
	})
	if lerr != nil {
		return nil, false, lerr
	}
	for i := uint64(first); i < s.count; i++ {
		e, err := r.entryAt(s, i)
		if err != nil {
			return nil, false, err
		}
		if e.hash != h {
			return nil, false, nil
		}
		k, err := r.str(s, e.keyOff, e.keyLen)
		if err != nil {
			return nil, false, err
		}
		switch c := bytes.Compare(k, key); {
		case c == 0:
			v, err := r.str(s, e.valOff, e.valLen)
			return v, err == nil, err
		case c > 0:
			return nil, false, nil
		}
	}
	return nil, false, nil
}

// Each walks a section in order. Entries and strings lie in that order, so
// both are read through buffers, not an entry at a time: a merge of a
// million-message base is two sequential reads. key and val are valid only
// during fn, which is what keeps a merge's memory off the base.
func (r *Reader) Each(id uint32, fn func(key, val []byte) error) error {
	s, ok := r.sections[id]
	if !ok {
		return nil
	}
	var rd io.ReaderAt = r.f
	if readAtSeam != nil {
		rd = readAtSeam(r.path, rd)
	}
	rd = strictReaderAt{rd}
	ents := bufio.NewReaderSize(io.NewSectionReader(rd, int64(s.entries), int64(s.count*entrySize)), 64<<10)
	strs := bufio.NewReaderSize(io.NewSectionReader(rd, int64(s.strings), int64(s.stringsLen)), 64<<10)
	var pos uint64
	b := make([]byte, entrySize)
	var kbuf, vbuf []byte
	next := func(buf *[]byte, off uint32, n uint16) ([]byte, error) {
		if uint64(off) != pos {
			return nil, fmt.Errorf("sortedfile: %w: strings out of order in %s", ErrMoved, r.path)
		}
		if cap(*buf) < int(n) {
			*buf = make([]byte, n, max(int(n), 256))
		}
		out := (*buf)[:n]
		if _, err := io.ReadFull(strs, out); err != nil {
			return nil, fmt.Errorf("sortedfile: %w: %w", ErrMoved, err)
		}
		pos += uint64(n)
		return out, nil
	}
	for i := uint64(0); i < s.count; i++ {
		if _, err := io.ReadFull(ents, b); err != nil {
			return fmt.Errorf("sortedfile: %w: %w", ErrMoved, err)
		}
		k, err := next(&kbuf, binary.LittleEndian.Uint32(b[8:]), binary.LittleEndian.Uint16(b[12:]))
		if err != nil {
			return err
		}
		v, err := next(&vbuf, binary.LittleEndian.Uint32(b[14:]), binary.LittleEndian.Uint16(b[18:]))
		if err != nil {
			return err
		}
		if err := fn(k, v); err != nil {
			return err
		}
	}
	return nil
}

// strictReaderAt makes a positioned read that comes back short an error: a
// short read with no error would have a buffered walk ask again for ever.
type strictReaderAt struct{ r io.ReaderAt }

func (s strictReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := s.r.ReadAt(p, off)
	if n < len(p) && (err == nil || errors.Is(err, io.EOF)) {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
