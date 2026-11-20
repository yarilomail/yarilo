package threads

import (
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/sortedfile"
)

func hexGUID(i int) string { return fmt.Sprintf("%032x", i) }

// mergeAt folds the log into the base when asked, the way a due merge does.
func mergeAt(t *testing.T, path string, merge bool) {
	t.Helper()
	if !merge {
		return
	}
	if err := MergeStore(path, direct); err != nil {
		t.Fatal(err)
	}
}

// A conversation holds maxThread messages; the next reply starts its own. The
// delivery and the rebuild's placement over a fold of the log agree, before
// and after a merge.
func TestAThreadStopsAtTheLimit(t *testing.T) {
	for _, merged := range []bool{false, true} {
		t.Run(fmt.Sprintf("merged %v", merged), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), FileName)
			rec := NewRecorder(NewCache(time.Minute, WithMaxThread(3)))
			root, err := rec.Record("u@x", path, hexGUID(0), rawMessage("m0@x", "", "", "Plan"))
			if err != nil {
				t.Fatal(err)
			}
			for i := 1; i < 3; i++ {
				got, err := rec.Record("u@x", path, hexGUID(i), rawMessage(fmt.Sprintf("m%d@x", i), "m0@x", "", "Re: Plan"))
				if err != nil || got != root {
					t.Fatalf("reply %d: %q %v, want %q", i, got, err, root)
				}
			}
			mergeAt(t, path, merged)
			raw := rawMessage("m3@x", "m0@x", "", "Re: Plan")
			st, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if p := PlacementFor(st, hexGUID(3), raw, 3); p.ThreadID != hexGUID(3) {
				t.Errorf("the rebuild placed the fourth into %q, want its own", p.ThreadID)
			}
			if p := PlacementFor(st, hexGUID(3), raw, 4); p.ThreadID != root {
				t.Errorf("under a limit of 4 the rebuild placed it into %q, want %q", p.ThreadID, root)
			}
			got, err := rec.Record("u@x", path, hexGUID(3), raw)
			if err != nil || got != hexGUID(3) {
				t.Errorf("delivery placed the fourth into %q %v, want its own", got, err)
			}
		})
	}
}

// Two conversations that together would pass the limit stay apart: the
// message joins the first and nothing is merged.
func TestAMergeOverTheLimitIsDropped(t *testing.T) {
	for _, merged := range []bool{false, true} {
		t.Run(fmt.Sprintf("merged %v", merged), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), FileName)
			rec := NewRecorder(NewCache(time.Minute, WithMaxThread(4)))
			a, _ := rec.Record("u@x", path, hexGUID(1), rawMessage("a@x", "", "", "One"))
			_, _ = rec.Record("u@x", path, hexGUID(2), rawMessage("a2@x", "a@x", "", "Re: One"))
			b, _ := rec.Record("u@x", path, hexGUID(3), rawMessage("b@x", "", "", "Two"))
			_, _ = rec.Record("u@x", path, hexGUID(4), rawMessage("b2@x", "b@x", "", "Re: Two"))
			mergeAt(t, path, merged)
			got, err := rec.Record("u@x", path, hexGUID(5), rawMessage("c@x", "", "<a@x> <b@x>", "Re: One"))
			if err != nil {
				t.Fatal(err)
			}
			st, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if tb, _ := st.ThreadOfGUID(hexGUID(3)); tb != b || got == b {
				t.Errorf("the second conversation was merged: message in %q, b now %q", got, tb)
			}
			if got != a && got != b {
				t.Errorf("the message went into %q, want one of the two", got)
			}
		})
	}
}

// A merge with an alias leaves the old conversation's members under the new
// one, and the old key is gone from the base's member lists.
func TestAMergedConversationsMembersMoveUnderTheWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	rec := NewRecorder(NewCache(time.Minute))
	a, _ := rec.Record("u@x", path, hexGUID(1), rawMessage("a@x", "", "", "One"))
	b, _ := rec.Record("u@x", path, hexGUID(2), rawMessage("b@x", "", "", "Two"))
	mergeAt(t, path, true) // both lists in the base before the alias
	late, err := rec.Record("u@x", path, hexGUID(3), rawMessage("c@x", "", "<a@x> <b@x>", "Re: One"))
	if err != nil {
		t.Fatal(err)
	}
	old := a
	if late == a {
		old = b
	}
	mergeAt(t, path, true)
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck
	members, err := s.Members(late)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(members)
	if want := []string{hexGUID(1), hexGUID(2), hexGUID(3)}; !slices.Equal(members, want) {
		t.Errorf("members of %q: %v, want %v", late, members, want)
	}
	if _, ok, err := s.base.Get(secMembers, []byte(old)); ok || err != nil {
		t.Errorf("the merged-away %q still has a member list: %v %v", old, ok, err)
	}
	if n, err := s.ThreadSize(late); err != nil || n != 3 {
		t.Errorf("size of %q: %d %v, want 3", late, n, err)
	}
}

// dropMembers rewrites the base without its member lists, as a base from
// before them was written.
func dropMembers(t *testing.T, path string) {
	t.Helper()
	base, err := sortedfile.Open(path + BaseSuffix)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close() //nolint:errcheck
	old := []uint32{secMessage, secSubject, secGUID}
	w, err := sortedfile.Create(path+BaseSuffix, base.Generation, base.Absorbed, old, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range old {
		if err := sortedfile.MergeSection(w, base, id, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
}

// A base without member lists builds them once from its GUIDs, and the lazy
// State's readers answer what the whole log folds to: members, sizes and the
// set of conversations, before and after that build.
func TestTheLazyStateAnswersWhatTheWholeLogFoldsTo(t *testing.T) {
	h := newHistory(t, 11)
	for i := 0; i < 300; i++ {
		h.place()
	}
	mergeAt(t, h.store, true)
	dropMembers(t, h.store)
	compare := func(step string) {
		t.Helper()
		want, err := Load(h.oracle)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Load(h.store)
		if err != nil {
			t.Fatal(err)
		}
		if got.pair == nil {
			t.Fatalf("%s: the store loaded whole, not as a pair", step)
		}
		wt, gt := want.Threads(), got.Threads()
		slices.Sort(wt)
		slices.Sort(gt)
		if !slices.Equal(wt, gt) {
			t.Fatalf("%s: %d conversations from the pair, %d from the whole log", step, len(gt), len(wt))
		}
		for _, id := range wt {
			wm, gm := want.GUIDsOfThread(id), got.GUIDsOfThread(id)
			slices.Sort(wm)
			slices.Sort(gm)
			if !slices.Equal(wm, gm) || got.ThreadSize(id) != want.ThreadSize(id) {
				t.Fatalf("%s: %q holds %v (size %d) from the pair, %v (size %d) from the whole log",
					step, id, gm, got.ThreadSize(id), wm, want.ThreadSize(id))
			}
		}
	}
	compare("a base without member lists")
	for i := 0; i < 50; i++ {
		h.place()
	}
	mergeAt(t, h.store, true)
	compare("after the merge built them")
}

// threadGetCost is what Thread/get of one conversation allocates on an
// account of n messages merged as deliveries leave it, with a cold cache.
func threadGetCost(t *testing.T, n int) uint64 {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	fill := NewRecorder(nil)
	for i := 0; i < n; i++ {
		reply := ""
		if i%3 != 0 {
			reply = fmt.Sprintf("m%d@x", i-i%3)
		}
		if _, err := fill.Record("u@x", path, hexGUID(i), rawMessage(fmt.Sprintf("m%d@x", i), reply, "", fmt.Sprintf("s %d", i/3))); err != nil {
			t.Fatal(err)
		}
		if fill.MergeDue(path) {
			mergeAt(t, path, true)
		}
	}
	mergeAt(t, path, true)
	cache := NewCache(time.Minute)
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	st, err := cache.Get("u@x", path)
	if err != nil {
		t.Fatal(err)
	}
	var members []string
	st.Read(func(v View) {
		if id, ok := v.ThreadOf(hexGUID(n / 2)); ok {
			members = v.Members(id)
		}
	})
	runtime.ReadMemStats(&m1)
	if len(members) != 3 {
		t.Fatalf("Thread/get found %d members, want 3", len(members))
	}
	return m1.TotalAlloc - m0.TotalAlloc
}

// Thread/get reads one conversation from the pair: what it allocates does not
// grow with the account (2000 against 10000 messages, 32 B a message).
func TestThreadGetDoesNotReadTheAccount(t *testing.T) {
	small, large := threadGetCost(t, 2000), threadGetCost(t, 10000)
	if grow := (float64(large) - float64(small)) / 8000; grow > 32 {
		t.Errorf("Thread/get allocates %d B at 2000, %d B at 10000: %.1f B a message", small, large, grow)
	}
}
