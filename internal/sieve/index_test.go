package sieve

import (
	"context"
	"errors"
	"testing"

	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/dict/memory"
)

func indexedStores(t *testing.T) map[string]*IndexedStore {
	t.Helper()
	d, err := memory.New(dict.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]*IndexedStore{
		"fs":   NewScriptStore("fs", "", nil, nil),
		"dict": NewScriptStore("redis", "", nil, d),
	}
}

func mustIndex(t *testing.T, s *IndexedStore, home string) ScriptIndex {
	t.Helper()
	ix, err := s.Index(context.Background(), "u", home)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

func TestScriptIndex(t *testing.T) {
	body := []byte("keep;\n")
	cases := []struct {
		name string
		run  func(t *testing.T, s *IndexedStore, home string)
	}{
		{"a rename keeps the id", func(t *testing.T, s *IndexedStore, home string) {
			ctx := context.Background()
			_ = s.SaveScript(ctx, "u", home, "a", body)
			id := mustIndex(t, s, home).IDs["a"]
			if err := s.RenameScript(ctx, "u", home, "a", "b"); err != nil {
				t.Fatal(err)
			}
			ix := mustIndex(t, s, home)
			if ix.IDs["b"] != id || len(ix.IDs) != 1 {
				t.Errorf("after rename ids = %v, want b to keep %s", ix.IDs, id)
			}
		}},
		{"a replaced script keeps its id and moves the state", func(t *testing.T, s *IndexedStore, home string) {
			ctx := context.Background()
			_ = s.SaveScript(ctx, "u", home, "a", body)
			before := mustIndex(t, s, home)
			_ = s.SaveScript(ctx, "u", home, "a", []byte("discard;\n"))
			after := mustIndex(t, s, home)
			if after.IDs["a"] != before.IDs["a"] || after.State <= before.State {
				t.Errorf("before %+v after %+v: want the same id and a higher state", before, after)
			}
		}},
		{"a deleted name comes back with a new id", func(t *testing.T, s *IndexedStore, home string) {
			ctx := context.Background()
			_ = s.SaveScript(ctx, "u", home, "a", body)
			old := mustIndex(t, s, home).IDs["a"]
			_ = s.DeleteScript(ctx, "u", home, "a")
			if ix := mustIndex(t, s, home); len(ix.IDs) != 0 {
				t.Fatalf("after delete ids = %v", ix.IDs)
			}
			_ = s.SaveScript(ctx, "u", home, "a", body)
			if id := mustIndex(t, s, home).IDs["a"]; id == old || id == "" {
				t.Errorf("recreated id %q, want a new one (old %q)", id, old)
			}
		}},
		{"activation moves the state", func(t *testing.T, s *IndexedStore, home string) {
			ctx := context.Background()
			_ = s.SaveScript(ctx, "u", home, "a", body)
			s0 := mustIndex(t, s, home).State
			_ = s.SetActive(ctx, "u", home, "a")
			s1 := mustIndex(t, s, home).State
			_ = s.Deactivate(ctx, "u", home)
			s2 := mustIndex(t, s, home).State
			if !(s0 < s1 && s1 < s2) {
				t.Errorf("states %d, %d, %d: want each change to move it", s0, s1, s2)
			}
		}},
		{"a script written behind the index gets an id once", func(t *testing.T, s *IndexedStore, home string) {
			ctx := context.Background()
			before := mustIndex(t, s, home)
			_ = s.ScriptStore.SaveScript(ctx, "u", home, "x", body)
			first := mustIndex(t, s, home)
			second := mustIndex(t, s, home)
			if first.IDs["x"] == "" || first.IDs["x"] != second.IDs["x"] || first.State != second.State {
				t.Errorf("first %+v second %+v: want one id and a reading that changes nothing", first, second)
			}
			if first.State <= before.State {
				t.Errorf("state %d -> %d: a script found behind the index is a change", before.State, first.State)
			}
		}},
		{"a script removed behind the index loses its id", func(t *testing.T, s *IndexedStore, home string) {
			ctx := context.Background()
			_ = s.SaveScript(ctx, "u", home, "x", body)
			_ = mustIndex(t, s, home)
			_ = s.ScriptStore.DeleteScript(ctx, "u", home, "x")
			if ix := mustIndex(t, s, home); len(ix.IDs) != 0 {
				t.Errorf("ids = %v, want none", ix.IDs)
			}
		}},
		{"a failed change leaves the state", func(t *testing.T, s *IndexedStore, home string) {
			ctx := context.Background()
			before := mustIndex(t, s, home).State
			if err := s.RenameScript(ctx, "u", home, "missing", "b"); err == nil {
				t.Fatal("renaming a missing script succeeded")
			}
			if after := mustIndex(t, s, home).State; after != before {
				t.Errorf("state %d -> %d after a failed rename", before, after)
			}
		}},
		{"a rename onto a stored name is refused and keeps both", func(t *testing.T, s *IndexedStore, home string) {
			ctx := context.Background()
			_ = s.SaveScript(ctx, "u", home, "a", body)
			_ = s.SaveScript(ctx, "u", home, "b", []byte("discard;\n"))
			before := mustIndex(t, s, home)
			if err := s.RenameScript(ctx, "u", home, "a", "b"); !errors.Is(err, ErrScriptExists) {
				t.Fatalf("rename onto b = %v, want ErrScriptExists", err)
			}
			for name, want := range map[string]string{"a": "keep;\n", "b": "discard;\n"} {
				if src, found, _ := s.GetScript(ctx, "u", home, name); !found || string(src) != want {
					t.Errorf("%s holds %q (found %v), want %q", name, src, found, want)
				}
			}
			if after := mustIndex(t, s, home); after.IDs["a"] != before.IDs["a"] || after.IDs["b"] != before.IDs["b"] || after.State != before.State {
				t.Errorf("index %+v -> %+v after a refused rename", before, after)
			}
		}},
		{"a create onto a stored name is refused", func(t *testing.T, s *IndexedStore, home string) {
			ctx := context.Background()
			_ = s.SaveScript(ctx, "u", home, "a", body)
			if err := s.CreateScript(ctx, "u", home, "a", []byte("discard;\n")); !errors.Is(err, ErrScriptExists) {
				t.Fatalf("create onto a = %v, want ErrScriptExists", err)
			}
			if src, _, _ := s.GetScript(ctx, "u", home, "a"); string(src) != string(body) {
				t.Errorf("a holds %q, want the stored %q", src, body)
			}
			if err := s.CreateScript(ctx, "u", home, "c", body); err != nil {
				t.Errorf("create of a new name: %v", err)
			}
		}},
		{"the index is not a script", func(t *testing.T, s *IndexedStore, home string) {
			ctx := context.Background()
			_ = s.SaveScript(ctx, "u", home, "a", body)
			names, err := s.ListScripts(ctx, "u", home)
			if err != nil || len(names) != 1 || names[0] != "a" {
				t.Errorf("ListScripts = %v (err %v), want [a]", names, err)
			}
		}},
	}
	for driver := range indexedStores(t) {
		for _, c := range cases {
			t.Run(driver+"/"+c.name, func(t *testing.T) {
				c.run(t, indexedStores(t)[driver], t.TempDir())
			})
		}
	}
}

// lockSpy marks the index lock as held while its fn runs.
type lockSpy struct{ held *bool }

func (l lockSpy) update(ctx context.Context, _, _ string, fn func(context.Context, *ScriptIndex) (bool, error)) (ScriptIndex, error) {
	ix := ScriptIndex{IDs: map[string]string{}}
	*l.held = true
	defer func() { *l.held = false }()
	_, err := fn(ctx, &ix)
	return ix, err
}

type listSpy struct {
	ScriptStore
	held, sawUnlocked *bool
}

func (l listSpy) ListScripts(context.Context, string, string) ([]string, error) {
	if !*l.held {
		*l.sawUnlocked = true
	}
	return nil, nil
}

// Index lists under the index lock: a listing between a rename and its entry
// would drop the old id and give the new name another.
func TestIndexListsUnderTheIndexLock(t *testing.T) {
	var held, sawUnlocked bool
	s := &IndexedStore{ScriptStore: listSpy{held: &held, sawUnlocked: &sawUnlocked}, io: lockSpy{held: &held}}
	if _, err := s.Index(context.Background(), "u", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if sawUnlocked {
		t.Error("ListScripts ran outside the index lock")
	}
}
