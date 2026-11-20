package threads

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
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
	// secMembers maps a thread to its members: their GUIDs, guidLen bytes
	// each, in the order they joined.
	secMembers
)

var baseSections = []uint32{secMessage, secSubject, secGUID, secMembers}

// guidLen is a GUID's length in a member list: 16 bytes in hex.
const guidLen = 32

// maxStoredMembers is the most one member list holds in the format's value.
const maxStoredMembers = 65535 / guidLen

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
	st.applyLocked(typ, key, val)
}

// applyLocked is apply for a caller holding st.mu, or one no reader sees yet.
func (st *State) applyLocked(typ, key, val string) {
	switch typ {
	case recMessage:
		st.byMessage[key] = val
	case recSubject:
		st.bySubject[key] = val
	case recGUID:
		if was, ok := st.byGUID[key]; ok && st.size[was] > 0 {
			st.size[was]--
		}
		st.byGUID[key] = val
		st.size[val]++
	case recAlias:
		st.aliasOf[key] = val
		if n := st.size[key]; n > 0 {
			st.size[val] += n
			delete(st.size, key)
		}
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
	resolve := func(_, v []byte) ([]byte, bool) {
		if _, aliased := s.tail.aliasOf[string(v)]; !aliased {
			return v, true
		}
		return []byte(s.resolve(string(v))), true
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
	// Members: what the log touched is read from the base by key beforehand,
	// so the merge holds those threads (each at most threading_max_thread),
	// not the base; a thread the log merged away drops on the way.
	members, dropped, merr := s.membersUpdates()
	if merr != nil {
		w.Abort()
		return merr
	}
	if err := sortedfile.MergeSectionMapped(w, s.base, secMembers, members, func(k, v []byte) ([]byte, bool) {
		return v, !dropped[string(k)]
	}); err != nil {
		w.Abort()
		return err
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

// baseMembers is a thread's member list in the base: guidLen bytes a member.
func (s *Store) baseMembers(thread string) ([]byte, error) {
	if s.base == nil {
		return nil, nil
	}
	if !s.noMembers() {
		v, _, err := s.base.Get(secMembers, []byte(thread))
		return v, err
	}
	// Until its first merge, a base from before the member lists answers from
	// its GUIDs, reading the section.
	var out []byte
	err := s.base.Each(secGUID, func(k, v []byte) error {
		if string(v) == thread && len(k) == guidLen {
			out = append(out, k...)
		}
		return nil
	})
	return out, err
}

// noMembers is a base written before the member lists.
func (s *Store) noMembers() bool {
	return s.base != nil && s.base.Count(secMembers) == 0 && s.base.Count(secGUID) > 0
}

// baseThreads walks the conversations the base names.
func (s *Store) baseThreads(fn func(thread string)) error {
	if s.base == nil {
		return nil
	}
	if s.noMembers() {
		return s.base.Each(secGUID, func(_, v []byte) error { fn(string(v)); return nil })
	}
	return s.base.Each(secMembers, func(k, _ []byte) error { fn(string(k)); return nil })
}

// absorbed lists the threads the log aliased into thread: their members in
// the base are thread's now.
func (s *Store) absorbed(thread string) []string {
	var out []string
	for old := range s.tail.aliasOf {
		if s.tail.resolve(old) == thread {
			out = append(out, old)
		}
	}
	return out
}

// ThreadSize is how many messages the conversation holds: its members in the
// base, those of the threads the log merged into it, and the log's own.
func (s *Store) ThreadSize(thread string) (int, error) {
	thread = s.resolve(thread)
	n := s.tail.size[thread]
	for _, t := range append(s.absorbed(thread), thread) {
		v, err := s.baseMembers(t)
		if err != nil {
			return 0, err
		}
		n += len(v) / guidLen
	}
	return n, nil
}

// Members lists the conversation's messages, by GUID: the base's, then those
// of the threads the log merged into it, then the log's.
func (s *Store) Members(thread string) ([]string, error) {
	thread = s.resolve(thread)
	var out []string
	for _, t := range append([]string{thread}, s.absorbed(thread)...) {
		v, err := s.baseMembers(t)
		if err != nil {
			return nil, err
		}
		for i := 0; i+guidLen <= len(v); i += guidLen {
			out = append(out, string(v[i:i+guidLen]))
		}
	}
	for guid, t := range s.tail.byGUID {
		if s.resolve(t) == thread {
			out = append(out, guid)
		}
	}
	return out, nil
}

// membersUpdates is what a merge writes into the members section: every
// thread the log touched, with its base members, those of the threads merged
// into it, and the log's; dropped names the merged threads, whose keys go.
func (s *Store) membersUpdates() (updates []sortedfile.KV, dropped map[string]bool, err error) {
	touched := map[string][]string{}
	for guid, t := range s.tail.byGUID {
		r := s.resolve(t)
		touched[r] = append(touched[r], guid)
	}
	// A base written before the members section has none: it is built once
	// from the GUID section, holding the account's members this one time.
	legacy := s.noMembers()
	if legacy {
		if err := s.base.Each(secGUID, func(k, v []byte) error {
			if len(k) != guidLen {
				return nil
			}
			r := s.resolve(string(v))
			touched[r] = append(touched[r], string(k))
			return nil
		}); err != nil {
			return nil, nil, err
		}
	}
	dropped = map[string]bool{}
	for old := range s.tail.aliasOf {
		dropped[old] = true
		r := s.resolve(old)
		if _, ok := touched[r]; !ok {
			touched[r] = nil
		}
	}
	for thread, fromLog := range touched {
		var v []byte
		for _, t := range append([]string{thread}, s.absorbed(thread)...) {
			if legacy {
				break // the GUID section's members are in fromLog already
			}
			b, err := s.baseMembers(t)
			if err != nil {
				return nil, nil, err
			}
			v = append(v, b...)
		}
		sort.Strings(fromLog)
		for _, g := range fromLog {
			v = append(v, g...)
		}
		if len(v)/guidLen > maxStoredMembers {
			slog.Warn("threads: a conversation longer than one member list holds keeps its first members",
				"path", s.path, "thread", thread, "members", len(v)/guidLen, "kept", maxStoredMembers)
			v = v[:maxStoredMembers*guidLen]
		}
		updates = append(updates, sortedfile.KV{Key: []byte(thread), Val: v})
	}
	return sortedfile.SortKVs(updates), dropped, nil
}
