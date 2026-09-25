//go:build flatcurve

package ftsservice

import (
	"sync"
	"testing"

	"github.com/yarilomail/yarilo/pkg/fts"
)

// countingSweepIndex records how often the sweep asked it anything.
type countingSweepIndex struct {
	boxesIndex
	mu     sync.Mutex
	sweeps int
}

func (i *countingSweepIndex) DropOrphanFolders(live []string) (int, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.sweeps++
	return 0, nil
}

// The index is the user's, so the sweep is: a whole-user compaction of an
// account with several folders sweeps once, not once per folder (#2026).
func TestOptimizeSweepsOnceForTheWholeUser(t *testing.T) {
	svc, box, uidx := newTestService(t)
	saveMessage(t, box, uidx, 1, "alpha")
	for _, name := range []string{"Archive", "Sent"} {
		if err := box.Create(name); err != nil {
			t.Fatal(err)
		}
	}
	idx := &countingSweepIndex{}
	idx.boxes = []fts.MailboxRef{{Name: "INBOX"}, {Name: "Archive"}, {Name: "Sent"}}
	h, err := svc.handle(testUser)
	if err != nil {
		t.Fatal(err)
	}
	h.ui = idx
	svc.release(h)

	if err := svc.Optimize(testUser); err != nil {
		t.Fatalf("optimize: %v", err)
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.sweeps != 1 {
		t.Errorf("the sweep ran %d times for 3 folders, want once", idx.sweeps)
	}
}
