//go:build flatcurve

package flatcurve

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A switch that stops part-way loses no document: every one is still found,
// and the next optimize leaves one shard holding them all (#2666).
func TestAnInterruptedSwitchLosesNoDocument(t *testing.T) {
	stopped := errors.New("stopped")
	tests := []struct {
		name string
		arm  func() func()
	}{
		{"a removal fails after the first shard is gone", func() func() {
			n := 0
			removeShard = func(p string) error {
				if n++; n > 1 {
					return stopped
				}
				return os.RemoveAll(p)
			}
			return func() { removeShard = os.RemoveAll }
		}},
		{"a retired shard is left with some of its files gone", func() func() {
			removeShard = func(p string) error {
				// All but one of its files, as a removal stopped near its end.
				files, _ := filepath.Glob(filepath.Join(p, "*"))
				for _, f := range files[min(1, len(files)):] {
					_ = os.Remove(f)
				}
				return stopped
			}
			return func() { removeShard = os.RemoveAll }
		}},
		{"a retired shard is left whole", func() func() {
			removeShard = func(string) error { return stopped }
			return func() { removeShard = os.RemoveAll }
		}},
		{"the process stops between the removals and the rename", func() func() {
			switchSeam = func() error { return stopped }
			return func() { switchSeam = nil }
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ui, _ := testEngine(t, testOpts(Options{RotateCount: 1}))
			for uid := uint32(1); uid <= 4; uid++ {
				indexDoc(t, ui, uid, nil, []string{"needle"})
			}
			if err := ui.Refresh(); err != nil {
				t.Fatal(err)
			}
			st := ui.(*userIndex).state()
			if paths, err := shardPaths(st.dir); err != nil || len(paths) < 3 {
				t.Fatalf("the fixture left %d shards (err %v), the row needs three", len(paths), err)
			}
			found := func(when string) {
				t.Helper()
				res, err := ui.Lookup([]string{inbox.GUID}, bodyQuery("needle"))
				if err != nil {
					t.Fatal(err)
				}
				if got := uidsOf(res.DefiniteGUIDs); !reflect.DeepEqual(got, []uint32{1, 2, 3, 4}) {
					t.Fatalf("%s: a search found %v, want 1 to 4", when, got)
				}
			}
			disarm := tc.arm()
			if err := ui.OptimizeMailbox(inbox); !errors.Is(err, stopped) {
				disarm()
				t.Fatalf("the optimize returned %v, want the stop", err)
			}
			disarm()
			found("after the stop")
			if err := ui.OptimizeMailbox(inbox); err != nil {
				t.Fatal(err)
			}
			if paths, err := shardPaths(st.dir); err != nil || len(paths) != 1 {
				t.Fatalf("the next optimize left %d shards (err %v), want one", len(paths), err)
			}
			if left, _ := filepath.Glob(filepath.Join(st.dir, removingPrefix+"*")); len(left) != 0 {
				t.Fatalf("the next optimize left %v behind", left)
			}
			found("after the next optimize")
		})
	}
}
