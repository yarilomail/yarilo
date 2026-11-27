package mdboxmap

import (
	"os"
	"sync"
	"testing"
)

// Two handles of one map in one process take turns: appends, a file rotation
// (a fold) and appends again. A map_uid handed out twice gives two bodies one
// name, and one of them is lost from the map (#2660).
func TestTwoHandlesNeverHandOutOneMapUID(t *testing.T) {
	a, dir := openTestMap(t)
	b, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close() //nolint:errcheck
	seen := map[uint32]string{}
	note := func(who string, uid uint32) {
		if prev, dup := seen[uid]; dup {
			t.Fatalf("map_uid %d handed out to %s and to %s", uid, prev, who)
		}
		seen[uid] = who
	}
	place := func(h *Map, who string) {
		if err := h.Place(func(p *Placement) error {
			fileID := p.HighestFileID()
			if fileID == 0 {
				fileID = 1
			}
			uid, err := p.AppendRecord(fileID, uint32(len(seen))*100, 100, [16]byte{byte(len(seen)), 1})
			if err != nil {
				return err
			}
			note(who, uid)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	rotate := func(h *Map, who string) {
		if err := h.Place(func(p *Placement) error {
			id, err := p.AllocFileID()
			if err != nil {
				return err
			}
			uid, err := p.AppendRecord(id, 0, 100, [16]byte{byte(len(seen)), 2})
			if err != nil {
				return err
			}
			note(who+" after rotating", uid)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	steps := []func(){
		func() { place(a, "a") }, func() { place(b, "b") }, func() { place(a, "a") },
		func() { rotate(a, "a") }, func() { place(b, "b") }, func() { place(a, "a") },
		func() { rotate(b, "b") }, func() { place(a, "a") }, func() { place(b, "b") },
		func() { _, _, _ = a.Lookup(1) }, func() { rotate(a, "a") }, func() { place(b, "b") },
	}
	for round := 0; round < 5; round++ {
		for _, s := range steps {
			s()
		}
	}
}

// The same under concurrency, with a log that folds every few kilobytes: on
// appends and on refcount changes, as an expunge makes them.
func TestConcurrentHandlesNeverHandOutOneMapUID(t *testing.T) {
	dir := t.TempDir()
	open := func() *Map {
		m, err := Open(dir, "alice@example.com", WithLogRotation(2048, 8192, 0))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.Close() })
		return m
	}
	handles := []*Map{open(), open(), open()}
	var mu sync.Mutex
	seen := map[uint32]int{}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for g := 0; g < 6; g++ {
		h := handles[g%len(handles)]
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			var mine []uint32
			for i := 0; i < 150; i++ {
				err := h.Place(func(p *Placement) error {
					fileID := p.HighestFileID()
					if i%25 == 0 {
						id, err := p.AllocFileID()
						if err != nil {
							return err
						}
						fileID = id
					}
					if fileID == 0 {
						fileID = 1
					}
					uid, err := p.AppendRecord(fileID, uint32(i)*100, 100, [16]byte{byte(g), byte(i), 3})
					if err != nil {
						return err
					}
					mu.Lock()
					seen[uid]++
					mu.Unlock()
					mine = append(mine, uid)
					return nil
				})
				if err != nil {
					errs <- err
					return
				}
				if i%3 == 0 && len(mine) > 0 {
					if err := h.UpdateRefcounts(mine[len(mine)-1:], -1); err != nil {
						errs <- err
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for uid, n := range seen {
		if n > 1 {
			t.Errorf("map_uid %d handed out %d times", uid, n)
		}
	}
	if len(seen) != 6*150 {
		t.Errorf("%d map_uids handed out, want %d", len(seen), 6*150)
	}
}

// A reader reloads between a fold's base rename and its log removal: it takes
// the new base and the old log's size. The writer then appends to a new log;
// the reader's next hold must replay that log from its start, not from the
// old log's size, or it hands out a map_uid the skipped records hold (#2660).
func TestAReloadInsideAFoldDoesNotSkipTheNewLog(t *testing.T) {
	// A new log as long as the old one reads as nothing changed; a longer one
	// replays only past the old one's size.
	for _, tc := range []struct {
		name  string
		after int
	}{{"as long as the old log", 2}, {"longer than the old log", 6}} {
		t.Run(tc.name, func(t *testing.T) {
			w, dir := openTestMap(t)
			r, err := Open(dir, "alice@example.com")
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close() //nolint:errcheck
			for i := 0; i < 2; i++ {
				if _, err := w.AppendRecord(1, uint32(i)*100, 100, [16]byte{byte(i), 4}); err != nil {
					t.Fatal(err)
				}
			}
			restore := SetTestLogRemoval(func(p string) error {
				if _, _, err := r.Lookup(9999); err != nil { // a miss reloads
					t.Errorf("reader lookup inside the fold: %v", err)
				}
				return os.Remove(p)
			})
			if _, err := w.AllocFileID(); err != nil {
				t.Fatal(err)
			}
			restore()
			var last uint32
			for i := 0; i < tc.after; i++ {
				if last, err = w.AppendRecord(2, uint32(i)*100, 100, [16]byte{byte(i), 5}); err != nil {
					t.Fatal(err)
				}
			}
			got, err := r.AppendRecord(2, 900, 100, [16]byte{9, 9})
			if err != nil {
				t.Fatal(err)
			}
			if got != last+1 {
				t.Fatalf("the reader handed out map_uid %d after the writer's %d", got, last)
			}
		})
	}
}

// A fold lands between a reload's checks and its replay: the log it then opens
// is of another lineage than its offset counts in, and that reload already
// takes the fold whole rather than leaving it to the next one.
func TestALogReplacedAfterTheCheckIsTakenByThatReload(t *testing.T) {
	w, dir := openTestMap(t)
	r, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close() //nolint:errcheck
	if _, err := w.AppendRecord(1, 0, 100, [16]byte{1, 6}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Lookup(1); err != nil { // r now counts in w's log
		t.Fatal(err)
	}
	if _, err := w.AppendRecord(1, 100, 100, [16]byte{2, 6}); err != nil {
		t.Fatal(err)
	}
	var last uint32
	reloadStatSeam = func() {
		reloadStatSeam = nil
		if _, err := w.AllocFileID(); err != nil {
			t.Error(err)
		}
		// One append: the new log ends where r's offset points.
		if last, err = w.AppendRecord(2, 0, 100, [16]byte{0, 7}); err != nil {
			t.Error(err)
		}
	}
	defer func() { reloadStatSeam = nil }()
	if _, _, err := r.Lookup(9999); err != nil { // a miss reloads, with no lock
		t.Fatal(err)
	}
	if got := r.NextMapUID(); got != last+1 {
		t.Fatalf("after the reload the reader would hand out map_uid %d, the writer's last is %d", got, last)
	}
}
