package locks

import (
	"sync"
)

// waitQueue orders the contenders for one resource by arrival. Without it the
// winner is whoever wakes first, and an exclusive contender loses to a series
// of short holders for as long as they keep arriving (#1821).
type waitQueue struct {
	mu sync.Mutex
	q  map[string][]*waiter
}

// waiter is one queued contender. ready is closed once, by whoever takes it
// off the front, so a waiter that has given up leaves nothing to signal.
type waiter struct {
	ready chan struct{}
}

func newWaitQueue() *waitQueue { return &waitQueue{q: make(map[string][]*waiter)} }

// join puts a contender at the back and returns it with its place in line;
// depth 0 means it is at the front and may try immediately.
func (wq *waitQueue) join(resource string) (*waiter, int) {
	w := &waiter{ready: make(chan struct{}, 1)}
	wq.mu.Lock()
	defer wq.mu.Unlock()
	depth := len(wq.q[resource])
	wq.q[resource] = append(wq.q[resource], w)
	if depth == 0 {
		w.ready <- struct{}{}
	}
	return w, depth
}

// leave removes a contender wherever it stands and hands its turn on when it
// was at the front: a waiter that timed out must not take the queue with it.
func (wq *waitQueue) leave(resource string, w *waiter) {
	wq.mu.Lock()
	defer wq.mu.Unlock()
	line := wq.q[resource]
	for i, entry := range line {
		if entry != w {
			continue
		}
		wq.q[resource] = append(line[:i:i], line[i+1:]...)
		if i == 0 {
			wq.signalFrontLocked(resource)
		}
		break
	}
	if len(wq.q[resource]) == 0 {
		delete(wq.q, resource)
	}
}

// wake lets the contender at the front try again. Called on every release, so
// a resource nobody is queued for costs one map lookup.
func (wq *waitQueue) wake(resource string) {
	wq.mu.Lock()
	defer wq.mu.Unlock()
	wq.signalFrontLocked(resource)
}

func (wq *waitQueue) signalFrontLocked(resource string) {
	line := wq.q[resource]
	if len(line) == 0 {
		return
	}
	select {
	case line[0].ready <- struct{}{}:
	default: // already told; one pending signal is enough
	}
}

// depth is how many contenders stand on a resource, for the metric and tests.
func (wq *waitQueue) depth(resource string) int {
	wq.mu.Lock()
	defer wq.mu.Unlock()
	return len(wq.q[resource])
}
