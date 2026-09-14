package locks

import (
	"context"
	"time"
)

// EventUnlocked is published on a resource when a lock on it is released, so
// contenders on every replica learn of it, not only those on the holder's.
const EventUnlocked EventType = "unlocked"

// ticketTTL is how long a queued ticket stands before it is taken for
// abandoned. A contender whose process died must not hold the line.
const ticketTTL = 2 * time.Minute

// WaitQueue is the arrival order of contenders on a resource. It lives in the
// Backend because the lock does: a queue per replica is no order at all (#1821).
type WaitQueue interface {
	// Enqueue puts ticket in line and returns how many stand ahead of it.
	Enqueue(ctx context.Context, resource, ticket string) (ahead int, err error)

	// AtFront reports whether ticket is the one whose turn it is.
	AtFront(ctx context.Context, resource, ticket string) (bool, error)

	// Dequeue takes ticket out of the line, wherever it stands.
	Dequeue(ctx context.Context, resource, ticket string) error

	// Wakes signals every time a lock on resource is released, wherever the
	// holder was. Cancel releases whatever the backend holds for it.
	Wakes(ctx context.Context, resource string) (signal <-chan struct{}, cancel func(), err error)
}

// queueing reports whether a backend carries the shared queue.
func queueing(b Backend) (WaitQueue, bool) {
	q, ok := b.(WaitQueue)
	return q, ok
}
