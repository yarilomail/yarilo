package locks

import (
	"context"
	"time"
)

// queued is one ticket and when it joined, so an abandoned one ages out the
// way the shared queue's does.
type queued struct {
	ticket string
	joined time.Time
}

// Enqueue implements WaitQueue.
func (b *MemoryBackend) Enqueue(_ context.Context, resource, ticket string) (int, error) {
	b.qmu.Lock()
	defer b.qmu.Unlock()
	line := b.pruneLocked(resource)
	for i, entry := range line {
		if entry.ticket == ticket {
			return i, nil
		}
	}
	b.queues[resource] = append(line, queued{ticket: ticket, joined: b.now()})
	return len(line), nil
}

// AtFront implements WaitQueue. An empty line answers yes: a contender whose
// ticket aged out should try, not wait for a turn nobody will hand it.
func (b *MemoryBackend) AtFront(_ context.Context, resource, ticket string) (bool, error) {
	b.qmu.Lock()
	defer b.qmu.Unlock()
	line := b.pruneLocked(resource)
	if len(line) == 0 {
		return true, nil
	}
	return line[0].ticket == ticket, nil
}

// Dequeue implements WaitQueue. Leaving names whoever now stands first, so a
// contender that gave up hands the turn on instead of leaving it to the timer.
func (b *MemoryBackend) Dequeue(_ context.Context, resource, ticket string) error {
	b.qmu.Lock()
	var nextUp string
	line := b.queues[resource]
	for i, entry := range line {
		if entry.ticket != ticket {
			continue
		}
		b.queues[resource] = append(line[:i:i], line[i+1:]...)
		break
	}
	if left := b.queues[resource]; len(left) == 0 {
		delete(b.queues, resource)
	} else {
		nextUp = left[0].ticket
	}
	b.qmu.Unlock()
	if nextUp != "" {
		b.mu.Lock()
		b.handOver(resource, nextUp)
		b.mu.Unlock()
	}
	return nil
}

// pruneLocked drops tickets older than the abandon window. Caller holds qmu.
func (b *MemoryBackend) pruneLocked(resource string) []queued {
	line := b.queues[resource]
	cutoff := b.now().Add(-ticketTTL)
	kept := line[:0]
	for _, entry := range line {
		if entry.joined.After(cutoff) {
			kept = append(kept, entry)
		}
	}
	b.queues[resource] = kept
	return kept
}

// Wakes implements WaitQueue.
func (b *MemoryBackend) Wakes(ctx context.Context, resource, ticket string) (<-chan struct{}, func(), error) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	if b.wakes[resource] == nil {
		b.wakes[resource] = make(map[chan struct{}]string)
	}
	b.wakes[resource][ch] = ticket
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		delete(b.wakes[resource], ch)
		if len(b.wakes[resource]) == 0 {
			delete(b.wakes, resource)
		}
	}, nil
}

// wakeLocked names whoever now stands first on resource. Caller holds b.mu.
func (b *MemoryBackend) wakeLocked(resource string) {
	b.qmu.Lock()
	line := b.queues[resource]
	var nextUp string
	if len(line) > 0 {
		nextUp = line[0].ticket
	}
	b.qmu.Unlock()
	b.handOver(resource, nextUp)
}

// handOver signals the listener whose ticket this turn names. Caller holds b.mu.
func (b *MemoryBackend) handOver(resource, ticket string) {
	for ch, want := range b.wakes[resource] {
		if ticket != "" && want != ticket {
			continue
		}
		select {
		case ch <- struct{}{}:
		default: // one pending signal is enough
		}
	}
}

// queuedAhead reports whether someone else holds the head of the queue: a
// caller with no ticket waits behind anyone who has one (#1809).
func (b *MemoryBackend) queuedAhead(resource, ticket string) (string, bool) {
	b.qmu.Lock()
	defer b.qmu.Unlock()
	line := b.pruneLocked(resource)
	if len(line) == 0 {
		return "", false
	}
	head := line[0].ticket
	return head, head != ticket
}
