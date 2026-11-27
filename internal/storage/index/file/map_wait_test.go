package file

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// What a reader waits for mapMu while another session writes: the lock is
// held over pointer swaps, never over the journal on disk, so the wait stays
// far under one change's write. Logged, not bounded: time is the stand's.
func TestAReaderWaitsForThePublicationLockOnlyBriefly(t *testing.T) {
	home := movedFolder(t, 3000)
	w, wf := acceptOpen(t, home)
	defer w.Close() //nolint:errcheck
	r, rf := acceptOpen(t, home)
	defer r.Close() //nolint:errcheck
	var mu sync.Mutex
	var waits []time.Duration
	mapWaitSeam = func(d time.Duration) { mu.Lock(); waits = append(waits, d); mu.Unlock() }
	defer func() { mapWaitSeam = nil }()

	var stop atomic.Bool
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				if _, err := r.GetMessages(rf.ID, mailbox.SeqSet{{From: 10, To: 20}}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	start := time.Now()
	const changes = 200
	for i := uint32(0); i < changes; i++ {
		if err := w.UpdateFlags(wf.ID, 1+i*13%2999, []string{`\Seen`}, nil); err != nil {
			t.Fatal(err)
		}
	}
	perChange := time.Since(start) / changes
	stop.Store(true)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(waits) == 0 {
		t.Fatal("no reader took the publication lock")
	}
	var total, worst time.Duration
	for _, d := range waits {
		total += d
		worst = max(worst, d)
	}
	t.Logf("%d reads under %d changes: mapMu wait mean %s, worst %s; one change takes %s",
		len(waits), changes, total/time.Duration(len(waits)), worst, perChange)
}
