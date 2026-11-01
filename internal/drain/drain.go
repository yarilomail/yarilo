// Package drain bounds a server's shutdown by its grace period: the grace is
// the longest a process waits for its connections, not a fixed sleep (#2433).
package drain

import (
	"sync"
	"time"
)

// Wait returns when every server in serving has returned, or at grace, and
// says whether they all did; a grace of zero or less does not wait.
func Wait(serving *sync.WaitGroup, grace time.Duration) bool {
	done := make(chan struct{})
	go func() {
		serving.Wait()
		close(done)
	}()
	if grace <= 0 {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}
