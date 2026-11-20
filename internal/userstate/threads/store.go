package threads

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/sortedfile"
)

// The sorted base under the log, and its sections (docs-internal
// THREADS-STORE.md): the log stays where it was and becomes the base's tail.
const BaseSuffix = ".base"

const (
	secMessage uint32 = 1 + iota
	secSubject
	secGUID
)

var baseSections = []uint32{secMessage, secSubject, secGUID}

// Store answers lookups from the base and its tail without folding the
// account: the tail is read whole, the base searched by key.
type Store struct {
	path string
	base *sortedfile.Reader
	tail *State
	// tailEnd is how far into the tail this store read; tailRecords how many
	// records it applied from it.
	tailEnd     int64
	tailRecords int
	// swapOwed says the log is the previous generation's: a merge renamed
	// this base and stopped before swapping the log.
	swapOwed bool
	// keepLog asks readTail to keep the records it applies, in order, for
	// ChangesSince; log holds them.
	keepLog bool
	log     []tailRecord
}

type tailRecord struct{ typ, key, val string }

// ErrPairBroken says the tail continues no base this store can open with it.
var ErrPairBroken = errors.New("threads: the log continues another base")

// pairAttempts bounds the reopens a reader makes when a merge finishes
// between its opening the base and reading the log.
const pairAttempts = 3

// errBaseBehind says the log is newer than the base this reader opened: a
// merge renamed a new base in between, and the pair is opened again.
var errBaseBehind = errors.New("threads: the log continues a newer base than the one opened")

// betweenOpens is a test seam: a merge finishing after the base is opened
// and before the log is read. Nil elsewhere.
var betweenOpens func()

// OpenStore opens the pair at path (the log) and path+BaseSuffix. No base is
// today's account: the log is the whole history. A log newer than the base
// opened is a base that moved in between: the pair is opened again, and only
// a mismatch that stays is ErrPairBroken.
func OpenStore(path string) (*Store, error) { return openStore(path, false) }

func openStore(path string, keepLog bool) (*Store, error) {
	for attempt := 1; ; attempt++ {
		s, err := openStoreOnce(path, keepLog)
		if !errors.Is(err, errBaseBehind) {
			return s, err
		}
		if attempt >= pairAttempts {
			return nil, fmt.Errorf("%w: %w", ErrPairBroken, err)
		}
	}
}

func openStoreOnce(path string, keepLog bool) (*Store, error) {
	s := &Store{path: path, keepLog: keepLog}
	base, err := sortedfile.Open(path + BaseSuffix)
	switch {
	case err == nil:
		s.base = base
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}
	if betweenOpens != nil {
		betweenOpens()
	}
	if err := s.readTail(); err != nil {
		s.Close() //nolint:errcheck
		return nil, err
	}
	return s, nil
}

// Close lets the base go.
func (s *Store) Close() error {
	if s.base == nil {
		return nil
	}
	return s.base.Close()
}

// readTail folds the log's records this base does not hold: all of them when
// the log continues this base, those past what the base absorbed when the
// log is the previous generation's (a merge stopped between its rename and
// the log's swap, §5).
func (s *Store) readTail() error {
	s.tail = Empty()
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("threads: open %s: %w", s.path, err)
	}
	defer f.Close() //nolint:errcheck
	rd := bufio.NewReaderSize(f, 64<<10)
	var off int64
	from := int64(0)
	first := true
	for {
		line, rerr := rd.ReadBytes('\n')
		start := off
		off += int64(len(line))
		if len(line) > 0 && line[len(line)-1] == '\n' {
			fields := bytes.Split(line[:len(line)-1], []byte("\t"))
			if first {
				first = false
				if s.base != nil {
					gen := uint64(0)
					if len(fields) == 3 && string(fields[0]) == recGen {
						gen, _ = strconv.ParseUint(string(fields[1]), 10, 64)
					}
					switch {
					case gen == s.base.Generation:
					case gen+1 == s.base.Generation:
						from = int64(s.base.Absorbed)
						s.swapOwed = true
					case gen > s.base.Generation:
						return fmt.Errorf("%w: log generation %d, base %d", errBaseBehind, gen, s.base.Generation)
					default:
						return fmt.Errorf("%w: log generation %d, base %d", ErrPairBroken, gen, s.base.Generation)
					}
				}
			}
			if start >= from && len(fields) == 3 {
				s.tail.apply(string(fields[0]), string(fields[1]), string(fields[2]))
				if string(fields[0]) != recGen {
					s.tailRecords++
					if s.keepLog {
						s.log = append(s.log, tailRecord{string(fields[0]), string(fields[1]), string(fields[2])})
					}
				}
			}
			s.tailEnd = off
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return fmt.Errorf("threads: read %s: %w", s.path, rerr)
		}
	}
}

// apply folds one record, as Load does.
func (st *State) apply(typ, key, val string) {
	switch typ {
	case recMessage:
		st.byMessage[key] = val
	case recSubject:
		st.bySubject[key] = val
	case recGUID:
		st.byGUID[key] = val
	case recAlias:
		st.aliasOf[key] = val
	case recGen:
		if g, err := strconv.ParseUint(key, 10, 64); err == nil {
			st.generation = g
		}
	}
	st.records++
}

// TailRecords is how many of the log's records this store applied.
func (s *Store) TailRecords() int { return s.tailRecords }

// lookup asks the tail, then the base; a base moved under it (ESTALE, a short
// read on NFS) opens the pair again and asks once more (THREADS-STORE.md §6).
func (s *Store) lookup(pick func(*State) map[string]string, sec uint32, key string) (string, bool, error) {
	v, ok, err := s.lookupOnce(pick(s.tail), sec, key)
	if !errors.Is(err, sortedfile.ErrMoved) {
		return v, ok, err
	}
	fresh, oerr := OpenStore(s.path)
	if oerr != nil {
		return "", false, oerr
	}
	s.Close() //nolint:errcheck
	*s = *fresh
	return s.lookupOnce(pick(s.tail), sec, key)
}

func (s *Store) lookupOnce(tail map[string]string, sec uint32, key string) (string, bool, error) {
	if v, ok := tail[key]; ok {
		return s.resolve(v), true, nil
	}
	if s.base == nil {
		return "", false, nil
	}
	v, ok, err := s.base.Get(sec, []byte(key))
	if err != nil || !ok {
		return "", false, err
	}
	return s.resolve(string(v)), true, nil
}

// resolve follows the tail's aliases: the base holds none, a merge folds
// them into the values it writes.
func (s *Store) resolve(id string) string { return s.tail.resolve(id) }

// ThreadOfMessage, ThreadOfSubject and ThreadOfGUID are State's lookups,
// from the pair.
func (s *Store) ThreadOfMessage(id string) (string, bool, error) {
	return s.lookup(func(t *State) map[string]string { return t.byMessage }, secMessage, id)
}

func (s *Store) ThreadOfSubject(key string) (string, bool, error) {
	return s.lookup(func(t *State) map[string]string { return t.bySubject }, secSubject, key)
}

func (s *Store) ThreadOfGUID(guid string) (string, bool, error) {
	return s.lookup(func(t *State) map[string]string { return t.byGUID }, secGUID, guid)
}

// MergeStore folds the log into a new base: built from a snapshot without
// the account's thread lock, so nobody waits for the rewrite; only the log's
// swap runs under hold, which takes the lock and copies what arrived since
// (THREADS-STORE.md §4, §5). The caller holds the merge's own lock.
func MergeStore(path string, hold func(func() error) error) error {
	// One merge a path in this process: the caller's lock covers the others.
	v, _ := merging.LoadOrStore(path, &sync.Mutex{})
	mu, _ := v.(*sync.Mutex)
	if !mu.TryLock() {
		return nil
	}
	defer mu.Unlock()
	basePath := path + BaseSuffix
	if _, err := sortedfile.RemoveTemps(basePath); err != nil {
		return err
	}
	if _, err := removeTailTemps(path); err != nil {
		return err
	}
	s, err := OpenStore(path)
	if err != nil {
		return err
	}
	// A merge that stopped after its base's rename owes the log its swap:
	// finished first, or this merge would write a base two generations past
	// that log, a pair nothing opens.
	if s.swapOwed {
		gen, absorbed := s.base.Generation, int64(s.base.Absorbed)
		s.Close() //nolint:errcheck
		if err := hold(func() error { return finishSwap(path, gen, absorbed) }); err != nil {
			return err
		}
		if s, err = OpenStore(path); err != nil {
			return err
		}
	}
	defer s.Close() //nolint:errcheck
	gen := s.tail.generation + 1
	if s.base != nil {
		gen = s.base.Generation + 1
	}
	w, err := sortedfile.Create(basePath, gen, uint64(s.tailEnd), baseSections, true)
	if err != nil {
		return err
	}
	// Only a value an alias names is rewritten: the rest pass as read, with
	// nothing allocated a base entry.
	resolve := func(v []byte) []byte {
		if _, aliased := s.tail.aliasOf[string(v)]; !aliased {
			return v
		}
		return []byte(s.resolve(string(v)))
	}
	for _, sec := range []struct {
		id uint32
		m  map[string]string
	}{{secMessage, s.tail.byMessage}, {secSubject, s.tail.bySubject}, {secGUID, s.tail.byGUID}} {
		kvs := make([]sortedfile.KV, 0, len(sec.m))
		for k, v := range sec.m {
			kvs = append(kvs, sortedfile.KV{Key: []byte(k), Val: []byte(v)})
		}
		if err := sortedfile.MergeSectionMapped(w, s.base, sec.id, sortedfile.SortKVs(kvs), resolve); err != nil {
			w.Abort()
			return err
		}
	}
	if err := w.Commit(); err != nil {
		return err
	}
	if mergeSwapSeam != nil {
		if err := mergeSwapSeam(); err != nil {
			return err
		}
	}
	return hold(func() error { return swapTail(path, gen, s.tailEnd) })
}

// finishSwap swaps the log a stopped merge left, under the caller's hold: only
// when the log is still the generation before gen, as another process may
// have finished it first.
func finishSwap(path string, gen uint64, absorbed int64) error {
	if logGeneration(path) != gen-1 {
		return nil
	}
	return swapTail(path, gen, absorbed)
}

// logGeneration is the generation the log's first record names; 0 for none.
func logGeneration(path string) uint64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close() //nolint:errcheck
	line, _ := bufio.NewReader(f).ReadString('\n')
	fields := bytes.Split([]byte(line), []byte("\t"))
	if len(fields) == 3 && string(fields[0]) == recGen {
		g, _ := strconv.ParseUint(string(fields[1]), 10, 64)
		return g
	}
	return 0
}

var merging sync.Map // path -> *sync.Mutex

// mergeSwapSeam is a test seam: a merge stopped after the base's rename and
// before the log's swap. Nil elsewhere.
var mergeSwapSeam func() error

// swapTail starts the log of generation gen with what the old one holds past
// from: the records a merge's snapshot did not see.
func swapTail(path string, gen uint64, from int64) error {
	old, err := os.Open(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("threads: open %s: %w", path, err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".swap.*")
	if err != nil {
		if old != nil {
			old.Close() //nolint:errcheck
		}
		return fmt.Errorf("threads: %w", err)
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()      //nolint:errcheck
		os.Remove(tmp) //nolint:errcheck
		if old != nil {
			old.Close() //nolint:errcheck
		}
		return fmt.Errorf("threads: swap %s: %w", path, err)
	}
	if _, err := f.WriteString(record(recGen, strconv.FormatUint(gen, 10), strconv.FormatInt(time.Now().Unix(), 10))); err != nil {
		return fail(err)
	}
	if old != nil {
		if _, err := old.Seek(from, io.SeekStart); err != nil {
			return fail(err)
		}
		if _, err := io.Copy(f, old); err != nil {
			return fail(err)
		}
		old.Close() //nolint:errcheck
		old = nil
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("threads: swap %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return fmt.Errorf("threads: swap %s: %w", path, err)
	}
	return nil
}

func removeTailTemps(path string) (int, error) {
	left, err := filepath.Glob(path + ".swap.*")
	if err != nil {
		return 0, fmt.Errorf("threads: %w", err)
	}
	for _, f := range left {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return 0, fmt.Errorf("threads: %w", err)
		}
	}
	return len(left), nil
}

// baseRecords is how many records the base stands for: the first positions of
// a Thread state in this generation (Load counts the same).
func (s *Store) baseRecords() int {
	if s.base == nil {
		return 0
	}
	n := 0
	for _, id := range baseSections {
		n += int(s.base.Count(id))
	}
	return n
}

// ErrBeforeBase says a position names records the base absorbed: what
// changed since then is folded away, and the client starts again.
var ErrBeforeBase = errors.New("threads: the position is before the base's records")

// changesSincePair is ChangesSince over a base and its log: the base's records
// come first, the log's after them, counted as Load counts them. A thread
// a G record names after its own message was created by it; any other was
// there before, in the base or the log (#2649).
func changesSincePair(path string, since int) (Changes, error) {
	s, err := openStore(path, true)
	if err != nil {
		return Changes{}, err
	}
	defer s.Close() //nolint:errcheck
	ch := Changes{Generation: s.base.Generation}
	n := s.baseRecords()
	if since < n {
		ch.Head = n + s.tailRecords
		return ch, ErrBeforeBase
	}
	known := map[string]bool{}
	created := map[string]bool{}
	updated := map[string]bool{}
	destroyed := map[string]bool{}
	for _, r := range s.log {
		n++
		if n <= since {
			switch r.typ {
			case recGUID:
				known[r.val] = true
			case recAlias:
				delete(known, r.key)
			}
			continue
		}
		switch r.typ {
		case recGUID:
			if known[r.val] || created[r.val] || r.val != r.key {
				if !created[r.val] {
					updated[r.val] = true
				}
			} else {
				created[r.val] = true
			}
			known[r.val] = true
		case recAlias:
			if created[r.key] {
				delete(created, r.key)
			} else {
				destroyed[r.key] = true
			}
			delete(updated, r.key)
			if !created[r.val] {
				updated[r.val] = true
			}
			known[r.val] = true
		}
	}
	ch.Head = n
	ch.Created = sortedKeys(created)
	ch.Updated = sortedKeys(updated)
	ch.Destroyed = sortedKeys(destroyed)
	return ch, nil
}
