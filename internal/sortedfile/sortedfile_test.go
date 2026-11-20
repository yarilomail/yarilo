package sortedfile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

func key(sec, i int) []byte { return []byte(fmt.Sprintf("<m%d.%d@example.org>", sec, i)) }
func val(i int) []byte      { return []byte(fmt.Sprintf("thread-%08d", i)) }

// build writes n entries in each of sections 1, 2 and 3 through the merge of
// an empty base, the way a first compaction writes them.
func build(t *testing.T, path string, n int, gen uint64) {
	t.Helper()
	w, err := Create(path, gen, 77, []uint32{1, 2, 3}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, sec := range []uint32{1, 2, 3} {
		kvs := make([]KV, n)
		for i := range kvs {
			kvs[i] = KV{key(int(sec), i), val(i)}
		}
		if err := MergeSection(w, nil, sec, SortKVs(kvs)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
}

// Every key is found under its section, none under another, a missing key is
// missing, and the header says what it was written with.
func TestEveryKeyIsFoundAndNoneThatIsNot(t *testing.T) {
	cases := []struct {
		name    string
		collide bool
	}{
		{name: "sha-256 prefix"},
		{name: "every key one hash", collide: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.collide {
				hashSeam = func([]byte) uint64 { return 7 }
				t.Cleanup(func() { hashSeam = nil })
			}
			path := filepath.Join(t.TempDir(), "f")
			build(t, path, 500, 3)
			r, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close() //nolint:errcheck
			if r.Generation != 3 || r.Absorbed != 77 || r.Count(2) != 500 {
				t.Fatalf("header: generation %d absorbed %d count %d", r.Generation, r.Absorbed, r.Count(2))
			}
			for i := 0; i < 500; i++ {
				v, ok, err := r.Get(2, key(2, i))
				if err != nil || !ok || string(v) != string(val(i)) {
					t.Fatalf("key %d: %q %v %v", i, v, ok, err)
				}
				if _, ok, _ := r.Get(1, key(2, i)); ok {
					t.Fatalf("key %d of section 2 found in section 1", i)
				}
			}
			if _, ok, err := r.Get(2, key(2, 500)); ok || err != nil {
				t.Fatalf("a missing key: %v %v", ok, err)
			}
			n, prev := 0, []byte(nil)
			if err := r.Each(3, func(k, _ []byte) error {
				if prev != nil && !Less(prev, k) {
					return fmt.Errorf("out of order at %d", n)
				}
				prev = append(prev[:0], k...)
				n++
				return nil
			}); err != nil || n != 500 {
				t.Fatalf("walked %d: %v", n, err)
			}
		})
	}
}

// A merge keeps the base's entries, replaces those the log changed and adds
// the new ones; the log's later word on a key is the one kept.
func TestAMergeTakesTheLogsLastWord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	build(t, path, 100, 1)
	base, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	updates := SortKVs([]KV{
		{key(1, 5), []byte("first")}, {key(1, 5), []byte("second")}, // the later stays
		{key(1, 200), []byte("new")},
	})
	w, err := Create(path, 2, 0, []uint32{1}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := MergeSection(w, base, 1, updates); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	base.Close() //nolint:errcheck
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close() //nolint:errcheck
	for _, c := range []struct {
		key  []byte
		want string
	}{{key(1, 5), "second"}, {key(1, 200), "new"}, {key(1, 6), string(val(6))}} {
		if v, ok, err := r.Get(1, c.key); err != nil || !ok || string(v) != c.want {
			t.Errorf("%s: %q %v %v, want %q", c.key, v, ok, err, c.want)
		}
	}
	if r.Count(1) != 101 {
		t.Errorf("%d entries, want 101", r.Count(1))
	}
}

// What a merge allocates does not grow with the base: it reads the base in
// order and holds the log's entries, not the base's.
func TestAMergeHoldsTheLogNotTheBase(t *testing.T) {
	alloc := func(n int) uint64 {
		dir := t.TempDir()
		path := filepath.Join(dir, "f")
		build(t, path, n, 1)
		base, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer base.Close() //nolint:errcheck
		updates := make([]KV, 1000)
		for i := range updates {
			updates[i] = KV{key(1, n+i), val(i)}
		}
		updates = SortKVs(updates)
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		w, err := Create(filepath.Join(dir, "g"), 2, 0, []uint32{1}, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := MergeSection(w, base, 1, updates); err != nil {
			t.Fatal(err)
		}
		if err := w.Commit(); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&m1)
		return m1.TotalAlloc - m0.TotalAlloc
	}
	small, large := alloc(20000), alloc(100000)
	if grow := (float64(large) - float64(small)) / 80000; grow > 8 {
		t.Errorf("a merge allocates %d B over 20000 entries, %d B over 100000: %.1f B an entry", small, large, grow)
	}
}

type failingReader struct {
	err  error
	cut  bool
	next io.ReaderAt
}

func (f failingReader) ReadAt(p []byte, off int64) (int, error) {
	if f.cut {
		n, _ := f.next.ReadAt(p[:len(p)/2], off)
		return n, nil
	}
	return 0, f.err
}

// A read that fails or comes back short, ESTALE on NFS among them, is the
// file having moved: the caller opens the pair again (INDEX-MEMORY.md §4).
func TestAFailedOrShortReadIsAMovedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	build(t, path, 50, 1)
	cases := []struct {
		name string
		rd   func(io.ReaderAt) io.ReaderAt
	}{
		{name: "ESTALE", rd: func(next io.ReaderAt) io.ReaderAt { return failingReader{err: syscall.ESTALE} }},
		{name: "short read", rd: func(next io.ReaderAt) io.ReaderAt { return failingReader{cut: true, next: next} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close() //nolint:errcheck
			readAtSeam = func(_ string, next io.ReaderAt) io.ReaderAt { return tc.rd(next) }
			defer func() { readAtSeam = nil }()
			if _, _, err := r.Get(1, key(1, 3)); !errors.Is(err, ErrMoved) {
				t.Errorf("a lookup answered %v, want ErrMoved", err)
			}
			if err := r.Each(1, func(_, _ []byte) error { return nil }); !errors.Is(err, ErrMoved) {
				t.Errorf("a walk answered %v, want ErrMoved", err)
			}
			if _, err := Open(path); !errors.Is(err, ErrMoved) {
				t.Errorf("an open answered %v, want ErrMoved", err)
			}
		})
	}
}

// Until the rename the old file is the file: a crash after the new one is
// durable and before its rename leaves readers on the old generation.
func TestTheOldFileStandsUntilTheRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	build(t, path, 10, 1)
	var seen uint64
	beforeRename = func(string) {
		r, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		seen = r.Generation
		r.Close() //nolint:errcheck
	}
	defer func() { beforeRename = nil }()
	build(t, path, 10, 2)
	if seen != 1 {
		t.Errorf("before the rename a reader opened generation %d, want 1", seen)
	}
	left, _ := filepath.Glob(path + ".*")
	if len(left) != 0 {
		t.Errorf("temps left behind: %v", left)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
