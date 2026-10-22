package sieve

import (
	"context"
	"reflect"
	"testing"

	"github.com/yarilomail/yarilo/internal/userstate/counters"
)

// Every script change moves SieveScript and VacationResponse, whichever store
// keeps the scripts; an index read that finds nothing new moves neither.
func TestScriptChangesMoveTheirTypes(t *testing.T) {
	both := []counters.Type{counters.SieveScript, counters.VacationResponse}
	for name, s := range indexedStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			home := t.TempDir()
			steps := []struct {
				name  string
				write func() error
				want  []counters.Type
			}{
				{"create", func() error { return s.CreateScript(ctx, "u", home, "a", []byte("keep;")) }, both},
				{"index unchanged", func() error { _, err := s.Index(ctx, "u", home); return err }, nil},
				{"save", func() error { return s.SaveScript(ctx, "u", home, "a", []byte("discard;")) }, both},
				{"activate", func() error { return s.SetActive(ctx, "u", home, "a") }, both},
				{"rename", func() error { return s.RenameScript(ctx, "u", home, "a", "b") }, both},
				{"delete", func() error { return s.DeleteScript(ctx, "u", home, "b") }, both},
			}
			for _, st := range steps {
				before, _ := counters.Read(counters.Path(home))
				if err := st.write(); err != nil {
					t.Fatalf("%s: %v", st.name, err)
				}
				after, _ := counters.Read(counters.Path(home))
				if got := after.ChangedAfter(before.Highest); !reflect.DeepEqual(got, st.want) {
					t.Errorf("%s moved %v, want %v", st.name, got, st.want)
				}
			}
		})
	}
}
