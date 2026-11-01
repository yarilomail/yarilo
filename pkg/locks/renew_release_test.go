package locks

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// renewLocker holds its first Renew mid-exchange until the hold is released
// or the row lets it go, and records a release that overtook it.
type renewLocker struct {
	refuse bool

	mu           sync.Mutex
	inFlight     bool
	released     bool
	overtaken    bool
	started      chan struct{}
	proceed      chan struct{}
	unlocked     chan struct{}
	startOnce    sync.Once
	proceedOnce  sync.Once
	unlockedOnce sync.Once
}

func newRenewLocker(refuse bool) *renewLocker {
	return &renewLocker{refuse: refuse, started: make(chan struct{}), proceed: make(chan struct{}), unlocked: make(chan struct{})}
}

func (r *renewLocker) let() { r.proceedOnce.Do(func() { close(r.proceed) }) }

func (r *renewLocker) Lock(context.Context, string, string, time.Duration) (Lock, error) {
	return Lock{ID: "l"}, nil
}
func (r *renewLocker) LockShared(ctx context.Context, res, o string, ttl time.Duration) (Lock, error) {
	return r.Lock(ctx, res, o, ttl)
}
func (r *renewLocker) Unlock(context.Context, string) error {
	r.mu.Lock()
	r.overtaken = r.overtaken || r.inFlight
	r.released = true
	r.mu.Unlock()
	r.unlockedOnce.Do(func() { close(r.unlocked) })
	r.let()
	return nil
}
func (r *renewLocker) Renew(context.Context, string, time.Duration) error {
	if r.refuse {
		return ErrExpired
	}
	first := false
	r.startOnce.Do(func() { first = true })
	if !first {
		return nil
	}
	r.mu.Lock()
	r.inFlight = true
	r.mu.Unlock()
	close(r.started)
	<-r.proceed
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inFlight = false
	if r.released {
		return ErrExpired
	}
	return nil
}
func (r *renewLocker) HoldsResource(string) (HoldMode, bool)                   { return HoldNone, false }
func (r *renewLocker) Close() error                                            { return nil }
func (r *renewLocker) Subscribe(context.Context, string) (<-chan Event, error) { return nil, nil }
func (r *renewLocker) Emit(context.Context, string, EventType, string) error   { return nil }
func (r *renewLocker) IncrementCounter(context.Context, string, int64) (int64, error) {
	return 0, nil
}

// A pass that ends while a renew is on the wire is a finished pass: the
// release waits for that renew, and only a renew refused before the release
// fails the hold (#2445).
func TestARenewInFlightIsNotOvertakenByTheRelease(t *testing.T) {
	cases := []struct {
		name    string
		refuse  bool
		wantErr error
	}{
		{name: "in flight when the pass ends", wantErr: nil},
		{name: "refused before the pass ends", refuse: true, wantErr: ErrExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := newRenewLocker(tc.refuse)
			// The renew is let go by the release that overtakes it, or, when
			// the release waits for it as it must, after a bound.
			go func() {
				select {
				case <-l.unlocked:
				case <-time.After(200 * time.Millisecond):
				}
				l.let()
			}()
			err := WithLock(WithSite(context.Background(), "write"), l, "r", Owner("u@example.com", NewID()), time.Minute, time.Millisecond, func(ctx context.Context) error {
				if tc.refuse {
					<-ctx.Done()
				} else {
					<-l.started
				}
				return nil
			})
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("WithLock = %v, want %v", err, tc.wantErr)
			}
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.overtaken {
				t.Error("the hold was released while its renew was still in flight")
			}
		})
	}
}
