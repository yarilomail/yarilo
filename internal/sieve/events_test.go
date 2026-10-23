package sieve

import (
	"context"
	"testing"

	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/dict/memory"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/locks/lockstest"
)

// Every script change is announced on the account's scripts, whichever store
// keeps them; an index read that finds nothing new is not.
func TestScriptChangesEmit(t *testing.T) {
	d, err := memory.New(dict.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, driver := range []string{"fs", "redis"} {
		t.Run(driver, func(t *testing.T) {
			rec := &lockstest.Recorder{}
			s := NewScriptStore(driver, "", rec, d)
			ctx := locks.WithID(context.Background(), "test")
			home := t.TempDir()
			steps := []struct {
				name  string
				write func() error
				emits bool
			}{
				{"create", func() error { return s.CreateScript(ctx, "u", home, "a", []byte("keep;")) }, true},
				{"index unchanged", func() error { _, err := s.Index(ctx, "u", home); return err }, false},
				{"save", func() error { return s.SaveScript(ctx, "u", home, "a", []byte("discard;")) }, true},
				{"activate", func() error { return s.SetActive(ctx, "u", home, "a") }, true},
				{"rename", func() error { return s.RenameScript(ctx, "u", home, "a", "b") }, true},
				{"delete", func() error { return s.DeleteScript(ctx, "u", home, "b") }, true},
			}
			for _, st := range steps {
				before := rec.Count(locks.SieveScriptsKey("u"), locks.EventSieve)
				if err := st.write(); err != nil {
					t.Fatalf("%s: %v", st.name, err)
				}
				if got := rec.Count(locks.SieveScriptsKey("u"), locks.EventSieve) - before; got != map[bool]int{true: 1}[st.emits] {
					t.Errorf("%s emitted %d, want emits=%v", st.name, got, st.emits)
				}
			}
		})
	}
}
