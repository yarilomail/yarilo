package maildir

import (
	"sync"
	"testing"
)

// The clock is the only field two backends in one process do not share, so a
// name minted while it stands still must still differ (#1953).
func TestMintNameTimeStepsWhenTheClockStandsStill(t *testing.T) {
	const names = 2000
	seen := make(map[[2]int64]bool, names)
	for i := 0; i < names; i++ {
		secs, usecs := mintNameTime()
		if seen[[2]int64{secs, usecs}] {
			t.Fatalf("name %d repeats the pair %d.M%d", i, secs, usecs)
		}
		seen[[2]int64{secs, usecs}] = true
	}
}

func TestMintNameTimeIsUniqueAcrossGoroutines(t *testing.T) {
	const workers, each = 8, 500
	var mu sync.Mutex
	seen := make(map[[2]int64]bool, workers*each)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				secs, usecs := mintNameTime()
				mu.Lock()
				if seen[[2]int64{secs, usecs}] {
					t.Errorf("two callers minted %d.M%d", secs, usecs)
				}
				seen[[2]int64{secs, usecs}] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
}
