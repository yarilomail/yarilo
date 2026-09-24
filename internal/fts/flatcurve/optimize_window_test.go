//go:build flatcurve

package flatcurve

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/pkg/fts"
)

// Row 7: the merge runs outside the caller's lock and the switch inside it.
// Holding the lock for the merge shuts deliveries out for its whole length;
// switching outside it races another writer's rename (#1986).
func TestTheMergeIsOutsideTheLockAndTheSwitchInside(t *testing.T) {
	ui, _ := testEngine(t, Options{RotateCount: 1})
	for uid := uint32(1); uid <= 3; uid++ {
		indexDoc(t, ui, uid, nil, []string{"needle"})
	}
	if err := ui.Refresh(); err != nil {
		t.Fatal(err)
	}
	st := ui.(*userIndex).state()
	before, err := shardPaths(st.dir)
	if err != nil || len(before) < 2 {
		t.Fatalf("the fixture left %d shards (err %v), the row needs at least two", len(before), err)
	}

	split, ok := ui.(fts.SplitOptimizer)
	if !ok {
		t.Fatal("this engine cannot separate the merge from the switch")
	}
	var mergedBeforeLock, switchedInsideLock bool
	err = split.OptimizeUnderLock(fts.MailboxRef{}, func(fn func() error) error {
		// Inside the lock: the merge is done and the shards are not yet
		// replaced.
		if _, serr := os.Stat(filepath.Join(st.dir, "optimize")); serr == nil {
			mergedBeforeLock = true
		}
		if err := fn(); err != nil {
			return err
		}
		paths, perr := shardPaths(st.dir)
		switchedInsideLock = perr == nil && len(paths) == 1
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !mergedBeforeLock {
		t.Error("the merged index did not exist when the lock was taken: the merge ran under it")
	}
	if !switchedInsideLock {
		t.Error("the shards were not replaced inside the lock: the switch ran outside it")
	}
}
