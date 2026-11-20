package threads

import (
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/sortedfile"
)

// history appends random placements, aliases among them, to the store's log
// and to an oracle log that is never merged, and remembers every key.
type history struct {
	t                    *testing.T
	rng                  *rand.Rand
	store, oracle        string
	storeSt, oracleSt    *State
	threads              []string // live: never merged away
	msgs, subjects, guid []string
	n                    int
}

func newHistory(t *testing.T, seed uint64) *history {
	dir := t.TempDir()
	return &history{t: t, rng: rand.New(rand.NewPCG(seed, seed+1)),
		store: filepath.Join(dir, "store", FileName), oracle: filepath.Join(dir, "oracle", FileName),
		storeSt: Empty(), oracleSt: Empty()}
}

func (h *history) place() {
	h.n++
	p := Placement{GUID: fmt.Sprintf("guid-%d", h.n), MessageID: fmt.Sprintf("<%d@x>", h.n)}
	if len(h.threads) > 0 && h.rng.IntN(3) > 0 {
		p.ThreadID = h.threads[h.rng.IntN(len(h.threads))]
	} else {
		p.ThreadID = "t-" + p.GUID
		h.threads = append(h.threads, p.ThreadID)
	}
	if h.rng.IntN(4) == 0 {
		p.SubjectKey = fmt.Sprintf("subject %d", h.rng.IntN(50))
		h.subjects = append(h.subjects, p.SubjectKey)
	}
	// As PlacementFor places: into a live thread, merging a live one, which is
	// then live no more.
	if len(h.threads) > 2 && h.rng.IntN(8) == 0 {
		i := h.rng.IntN(len(h.threads))
		if old := h.threads[i]; old != p.ThreadID {
			p.MergedFrom = []string{old}
			h.threads = append(h.threads[:i], h.threads[i+1:]...)
		}
	}
	h.msgs, h.guid = append(h.msgs, p.MessageID), append(h.guid, p.GUID)
	for _, pair := range []struct {
		path string
		st   *State
	}{{h.store, h.storeSt}, {h.oracle, h.oracleSt}} {
		if err := Append(pair.path, pair.st, p); err != nil {
			h.t.Fatal(err)
		}
	}
}

// check compares every key the history wrote between the store and the
// oracle, folded whole as today.
func (h *history) check(step string) {
	h.t.Helper()
	want, err := Load(h.oracle)
	if err != nil {
		h.t.Fatal(err)
	}
	s, err := OpenStore(h.store)
	if err != nil {
		h.t.Fatalf("%s: %v", step, err)
	}
	defer s.Close() //nolint:errcheck
	for _, c := range []struct {
		keys []string
		got  func(string) (string, bool, error)
		want func(string) (string, bool)
	}{
		{h.msgs, s.ThreadOfMessage, want.ThreadOfMessage},
		{h.subjects, s.ThreadOfSubject, want.ThreadOfSubject},
		{h.guid, s.ThreadOfGUID, want.ThreadOfGUID},
	} {
		for _, k := range c.keys {
			g, gok, gerr := c.got(k)
			w, wok := c.want(k)
			if gerr != nil || g != w || gok != wok {
				h.t.Fatalf("%s: %q answers %q %v %v from the pair, %q %v from the whole log", step, k, g, gok, gerr, w, wok)
			}
		}
	}
}

func direct(fn func() error) error { return fn() }

// Every key answers from the pair what the whole log folds to, through random
// histories with merges at random points, aliases folded on the way.
func TestThePairAnswersWhatTheWholeLogFoldsTo(t *testing.T) {
	for seed := uint64(1); seed <= 5; seed++ {
		h := newHistory(t, seed)
		for step := 0; step < 400; step++ {
			h.place()
			if h.rng.IntN(60) == 0 {
				if err := MergeStore(h.store, direct); err != nil {
					t.Fatalf("seed %d step %d: merge: %v", seed, step, err)
				}
			}
			if step%50 == 49 {
				h.check(fmt.Sprintf("seed %d step %d", seed, step))
			}
		}
	}
}

// A merge that stopped after the base's rename and before the log's swap
// leaves the old log beside the new base: a reader applies only the records
// past what the base absorbed, none twice, and loses none written after.
func TestAMergeStoppedBeforeTheSwapLosesAndRepeatsNothing(t *testing.T) {
	h := newHistory(t, 9)
	for i := 0; i < 100; i++ {
		h.place()
	}
	mergeSwapSeam = func() error { return errors.New("stopped before the swap") }
	err := MergeStore(h.store, direct)
	mergeSwapSeam = nil
	if err == nil {
		t.Fatal("the stopped merge reported success")
	}
	for i := 0; i < 7; i++ {
		h.place()
	}
	s, err := OpenStore(h.store)
	if err != nil {
		t.Fatal(err)
	}
	applied := s.TailRecords()
	s.Close() //nolint:errcheck
	if want := 7 * 2; applied < want || applied > want+7*3 {
		t.Errorf("the reader applied %d records of the old log, want only the %d-odd written after the merge", applied, want)
	}
	h.check("after the stopped merge")
	if err := MergeStore(h.store, direct); err != nil {
		t.Fatal(err)
	}
	h.check("after the next merge")
}

// A merge clears the temps one that stopped left, base-sized files otherwise
// left in the user's directory.
func TestAMergeClearsWhatAStoppedOneLeft(t *testing.T) {
	h := newHistory(t, 3)
	for i := 0; i < 20; i++ {
		h.place()
	}
	for _, name := range []string{h.store + BaseSuffix + ".tmp.1", h.store + BaseSuffix + ".str.1", h.store + ".swap.1"} {
		if err := os.WriteFile(name, []byte("left"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := MergeStore(h.store, direct); err != nil {
		t.Fatal(err)
	}
	left, _ := filepath.Glob(h.store + "*.*")
	for _, f := range left {
		if f != h.store+BaseSuffix {
			t.Errorf("left behind: %s", f)
		}
	}
	h.check("after the merge")
}

type badRead struct {
	short bool
	next  io.ReaderAt
}

func (b badRead) ReadAt(p []byte, off int64) (int, error) {
	if b.short {
		return b.next.ReadAt(p[:len(p)/2], off)
	}
	return 0, syscall.ESTALE
}

// A base whose read fails or comes back short, as one replaced under a reader
// on NFS, sends the lookup to open the pair again; the answer is the
// current pair's.
func TestALookupOnAMovedBaseOpensThePairAgain(t *testing.T) {
	for _, short := range []bool{false, true} {
		t.Run(fmt.Sprintf("short %v", short), func(t *testing.T) {
			h := newHistory(t, 4)
			for i := 0; i < 30; i++ {
				h.place()
			}
			if err := MergeStore(h.store, direct); err != nil {
				t.Fatal(err)
			}
			s, err := OpenStore(h.store)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close() //nolint:errcheck
			fails := 1
			sortedfile.SetReadSeam(func(_ string, next io.ReaderAt) io.ReaderAt {
				if fails > 0 {
					fails--
					return badRead{short: short, next: next}
				}
				return next
			})
			defer sortedfile.SetReadSeam(nil)
			want, _ := h.oracleSt.ThreadOfGUID(h.guid[3])
			if got, ok, err := s.ThreadOfGUID(h.guid[3]); err != nil || !ok || got != want {
				t.Errorf("after a moved base: %q %v %v, want %q", got, ok, err, want)
			}
		})
	}
}

// A log that continues neither this base nor the one before it is not read
// against it.
func TestALogOfAnotherGenerationIsRefused(t *testing.T) {
	h := newHistory(t, 5)
	for i := 0; i < 10; i++ {
		h.place()
	}
	if err := MergeStore(h.store, direct); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.store, []byte(record(recGen, "40", "0")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(h.store); !errors.Is(err, ErrPairBroken) {
		t.Errorf("a log of generation 40 opened with %v, want ErrPairBroken", err)
	}
}
