package sortedfile

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
)

// Writer streams a new file: sections in the order given, each one's
// entries in the file's order. It holds buffers, not the entries: a section's
// strings go to a temp file beside it and are copied in after its entries.
type Writer struct {
	path, tmp string
	f         *os.File
	bw        *bufio.Writer
	off       uint64
	gen, abs  uint64
	ids       []uint32
	done      []section
	cur       *section
	strs      *os.File
	sw        *bufio.Writer
	strLen    uint64
	last      []byte
	ent       [entrySize]byte
	fsync     bool
}

// Create starts a file that will replace path on Commit, holding the
// sections ids in that order.
func Create(path string, generation, absorbed uint64, ids []uint32, fsync bool) (*Writer, error) {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return nil, fmt.Errorf("sortedfile: %w", err)
	}
	w := &Writer{path: path, tmp: f.Name(), f: f, gen: generation, abs: absorbed, ids: ids, fsync: fsync}
	w.off = uint64(headFixed + len(ids)*sectionLen)
	if _, err := f.Seek(int64(w.off), io.SeekStart); err != nil {
		w.Abort()
		return nil, fmt.Errorf("sortedfile: %w", err)
	}
	w.bw = bufio.NewWriterSize(f, 64<<10)
	return w, nil
}

// Begin opens the next section; it must be the next of the ids given.
func (w *Writer) Begin(id uint32) error {
	if w.cur != nil || len(w.done) >= len(w.ids) || w.ids[len(w.done)] != id {
		return fmt.Errorf("sortedfile: section %d out of turn", id)
	}
	s, err := os.CreateTemp(filepath.Dir(w.path), filepath.Base(w.path)+".str.*")
	if err != nil {
		return fmt.Errorf("sortedfile: %w", err)
	}
	w.strs, w.sw, w.strLen, w.last = s, bufio.NewWriterSize(s, 64<<10), 0, nil
	w.cur = &section{id: id, entries: w.off}
	return nil
}

var errOrder = errors.New("sortedfile: entries out of order")

// Add appends one entry; keys come in the file's order, each once.
func (w *Writer) Add(key, val []byte) error {
	if w.cur == nil {
		return fmt.Errorf("sortedfile: an entry outside a section")
	}
	if len(key) > math.MaxUint16 || len(val) > math.MaxUint16 {
		return fmt.Errorf("sortedfile: an entry of %d and %d bytes is too long", len(key), len(val))
	}
	if w.last != nil && !Less(w.last, key) {
		return errOrder
	}
	w.last = append(w.last[:0], key...)
	if w.strLen+uint64(len(key)+len(val)) > math.MaxUint32 {
		return fmt.Errorf("sortedfile: section %d's strings pass 4 GiB", w.cur.id)
	}
	b := w.ent[:]
	binary.LittleEndian.PutUint64(b[0:], hashOf(key))
	binary.LittleEndian.PutUint32(b[8:], uint32(w.strLen))
	binary.LittleEndian.PutUint16(b[12:], uint16(len(key)))
	binary.LittleEndian.PutUint32(b[14:], uint32(w.strLen)+uint32(len(key)))
	binary.LittleEndian.PutUint16(b[18:], uint16(len(val)))
	if _, err := w.bw.Write(b); err != nil {
		return fmt.Errorf("sortedfile: %w", err)
	}
	if _, err := w.sw.Write(key); err != nil {
		return fmt.Errorf("sortedfile: %w", err)
	}
	if _, err := w.sw.Write(val); err != nil {
		return fmt.Errorf("sortedfile: %w", err)
	}
	w.strLen += uint64(len(key) + len(val))
	w.cur.count++
	w.off += entrySize
	return nil
}

// End closes the section, its strings copied in after its entries.
func (w *Writer) End() error {
	if w.cur == nil {
		return fmt.Errorf("sortedfile: no section open")
	}
	if err := w.sw.Flush(); err != nil {
		return fmt.Errorf("sortedfile: %w", err)
	}
	if _, err := w.strs.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("sortedfile: %w", err)
	}
	if _, err := io.Copy(w.bw, w.strs); err != nil {
		return fmt.Errorf("sortedfile: %w", err)
	}
	name := w.strs.Name()
	w.strs.Close()  //nolint:errcheck
	os.Remove(name) //nolint:errcheck
	w.strs = nil
	w.cur.strings, w.cur.stringsLen = w.off, w.strLen
	w.off += w.strLen
	w.done = append(w.done, *w.cur)
	w.cur = nil
	return nil
}

// Commit writes the header, makes the file durable when asked, and renames
// it over path.
func (w *Writer) Commit() error {
	if w.cur != nil || len(w.done) != len(w.ids) {
		w.Abort()
		return fmt.Errorf("sortedfile: %d of %d sections written", len(w.done), len(w.ids))
	}
	if err := w.bw.Flush(); err != nil {
		w.Abort()
		return fmt.Errorf("sortedfile: %w", err)
	}
	head := make([]byte, headFixed+len(w.done)*sectionLen)
	copy(head, magic)
	binary.LittleEndian.PutUint32(head[4:], version)
	binary.LittleEndian.PutUint64(head[8:], w.gen)
	binary.LittleEndian.PutUint64(head[16:], w.abs)
	binary.LittleEndian.PutUint32(head[24:], uint32(len(w.done)))
	for i, s := range w.done {
		b := head[headFixed+i*sectionLen:]
		binary.LittleEndian.PutUint32(b[0:], s.id)
		binary.LittleEndian.PutUint64(b[4:], s.count)
		binary.LittleEndian.PutUint64(b[12:], s.entries)
		binary.LittleEndian.PutUint64(b[20:], s.strings)
		binary.LittleEndian.PutUint64(b[28:], s.stringsLen)
	}
	if _, err := w.f.WriteAt(head, 0); err != nil {
		w.Abort()
		return fmt.Errorf("sortedfile: %w", err)
	}
	if w.fsync {
		if err := w.f.Sync(); err != nil {
			w.Abort()
			return fmt.Errorf("sortedfile: %w", err)
		}
	}
	if err := w.f.Close(); err != nil {
		os.Remove(w.tmp) //nolint:errcheck
		return fmt.Errorf("sortedfile: %w", err)
	}
	if beforeRename != nil {
		beforeRename(w.tmp)
	}
	if err := os.Rename(w.tmp, w.path); err != nil {
		os.Remove(w.tmp) //nolint:errcheck
		return fmt.Errorf("sortedfile: %w", err)
	}
	return nil
}

// Abort leaves path as it was.
func (w *Writer) Abort() {
	if w.strs != nil {
		name := w.strs.Name()
		w.strs.Close()  //nolint:errcheck
		os.Remove(name) //nolint:errcheck
	}
	w.f.Close()      //nolint:errcheck
	os.Remove(w.tmp) //nolint:errcheck
}

// beforeRename is a test seam: a crash between the durable temp and its
// rename. Nil elsewhere.
var beforeRename func(tmp string)
