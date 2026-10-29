// Package lockstest holds lock service doubles for tests.
package lockstest

import (
	"context"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/locks"
)

// Recorder grants every lock at once and records every emitted event as
// "resource type".
type Recorder struct {
	mu     sync.Mutex
	events []string
}

var _ locks.Locker = (*Recorder)(nil)

// Count is how many times the event was emitted on the resource.
func (r *Recorder) Count(resource string, t locks.EventType) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e == resource+" "+string(t) {
			n++
		}
	}
	return n
}

// Lock implements locks.Locker.
func (r *Recorder) Lock(_ context.Context, resource, owner string, _ time.Duration) (locks.Lock, error) {
	return locks.Lock{ID: resource, Resource: resource, Owner: owner}, nil
}

// LockShared implements locks.Locker.
func (r *Recorder) LockShared(ctx context.Context, resource, owner string, ttl time.Duration) (locks.Lock, error) {
	return r.Lock(ctx, resource, owner, ttl)
}

// Unlock implements locks.Locker.
func (r *Recorder) Unlock(context.Context, string) error { return nil }

// Renew implements locks.Locker.
func (r *Recorder) Renew(context.Context, string, time.Duration) error { return nil }

// Subscribe implements locks.Locker.
func (r *Recorder) Subscribe(context.Context, string) (<-chan locks.Event, error) {
	return make(chan locks.Event), nil
}

// Emit records the event as "resource type".
func (r *Recorder) Emit(_ context.Context, resource string, t locks.EventType, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, resource+" "+string(t))
	return nil
}

// HoldsResource implements locks.Locker.
func (r *Recorder) HoldsResource(string) (locks.HoldMode, bool) { return locks.HoldNone, false }

// IncrementCounter implements locks.Locker.
func (r *Recorder) IncrementCounter(context.Context, string, int64) (int64, error) { return 0, nil }

// Close implements locks.Locker.
func (r *Recorder) Close() error { return nil }
