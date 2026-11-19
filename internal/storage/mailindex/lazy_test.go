package mailindex

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func lazyBase(t *testing.T, path string, n int, flags MailFlag) { //nolint:unparam
	t.Helper()
	f, err := NewFile(1717185600, []Extension{{Name: "guid", RecordSize: 16, RecordAlign: 4}})
	if err != nil {
		t.Fatal(err)
	}
	f.Header.NextUID = uint32(n + 1)
	f.Header.MessagesCount = uint32(n)
	for i := 0; i < n; i++ {
		g := make([]byte, 16)
		g[0] = byte(i)
		f.Records = append(f.Records, &Record{UID: uint32(i + 1), Flags: flags, Ext: map[string][]byte{"guid": g}})
	}
	if _, err := Recreate(f.ToRecreateInput(path)); err != nil {
		t.Fatal(err)
	}
}

// A lazy base loads only the chunks asked for, and reads as the whole one.
func TestLazyRecordsReadAsTheBaseHolds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "i.index")
	n := 3*ChunkRecords + 5
	lazyBase(t, path, n, FlagSeen)
	whole, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	f, r, err := OpenLazy(path)
	if err != nil {
		t.Fatal(err)
	}
	if f.Records != nil || r.Len() != n {
		t.Fatalf("lazy open decoded %d records, holds %d of %d", len(f.Records), r.Len(), n)
	}
	if err := r.Load(ChunkRecords+3, ChunkRecords+4); err != nil {
		t.Fatal(err)
	}
	for ci, want := range []bool{false, true, false, false} {
		if r.Loaded(ci) != want {
			t.Errorf("chunk %d loaded %v after a load of one record of chunk 1", ci, r.Loaded(ci))
		}
	}
	got, err := r.Decode()
	if err != nil {
		t.Fatal(err)
	}
	for i := range got {
		if got[i].UID != whole.Records[i].UID || got[i].Flags != whole.Records[i].Flags || !bytes.Equal(got[i].Ext["guid"], whole.Records[i].Ext["guid"]) {
			t.Fatalf("record %d: %+v, want %+v", i, got[i], whole.Records[i])
		}
	}
}

type failingReader struct {
	io.ReaderAt
	err   error
	short bool
}

func (f failingReader) ReadAt(p []byte, off int64) (int, error) {
	if off == 0 {
		return f.ReaderAt.ReadAt(p, off)
	}
	if f.short {
		n, _ := f.ReaderAt.ReadAt(p[:len(p)/2], off)
		return n, io.EOF
	}
	return 0, f.err
}

// A chunk load from a base that another process replaced is ErrBaseMoved,
// whichever way the replacement shows (INDEX-MEMORY.md §4).
func TestLazyLoadOfAMovedBaseSaysSo(t *testing.T) {
	cases := []struct {
		name    string
		replace func(t *testing.T, path string)
		seam    func(string, io.ReaderAt) io.ReaderAt
	}{
		{name: "renamed over, the same header", replace: func(t *testing.T, path string) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			raw[len(raw)-1] ^= 0xff
			if err := os.WriteFile(path+".new", raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(path+".new", path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "rewritten in place", replace: func(t *testing.T, path string) {
			f, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteAt([]byte{0x7f}, 40); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "removed", replace: func(t *testing.T, path string) { os.Remove(path) }}, //nolint:errcheck
		{name: "stale handle", seam: func(_ string, r io.ReaderAt) io.ReaderAt { return failingReader{ReaderAt: r, err: syscall.ESTALE} }},
		{name: "short read", seam: func(_ string, r io.ReaderAt) io.ReaderAt { return failingReader{ReaderAt: r, short: true} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "i.index")
			lazyBase(t, path, 2*ChunkRecords, FlagSeen)
			_, r, err := OpenLazy(path)
			if err != nil {
				t.Fatal(err)
			}
			if c.replace != nil {
				c.replace(t, path)
			}
			ChunkReadSeam = c.seam
			defer func() { ChunkReadSeam = nil }()
			err = r.Load(0, r.Len())
			if !errors.Is(err, ErrBaseMoved) {
				t.Fatalf("load: %v, want ErrBaseMoved", err)
			}
			if r.Loaded(0) || r.Loaded(1) {
				t.Error("a chunk of the moved base was kept")
			}
		})
	}
}

func openFDs(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skip("no /dev/fd")
	}
	return len(ents)
}

// A lazy base keeps no descriptor between loads: many open bases cost no
// files.
func TestLazyBasesKeepNoDescriptor(t *testing.T) {
	dir := t.TempDir()
	var held []*Records
	before := openFDs(t)
	for i := 0; i < 100; i++ {
		path := filepath.Join(dir, "i"+string(rune('a'+i%26))+string(rune('a'+i/26))+".index")
		lazyBase(t, path, ChunkRecords+1, FlagSeen)
		_, r, err := OpenLazy(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Load(0, 1); err != nil {
			t.Fatal(err)
		}
		held = append(held, r)
	}
	if after := openFDs(t); after != before {
		t.Errorf("%d descriptors open after 100 lazy bases, %d before", after, before)
	}
	_ = held
}
