package locks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// MemoryBackend is the in-memory state backend used by embedded mode
// (standalone deployments). State is ephemeral: on process crash all locks
// are lost, which is acceptable for standalone where every session process
// restarts with the pod.
type MemoryBackend struct {
	mu        sync.Mutex
	locks     map[string]*memLock                // by lockID, exclusive AND shared
	byRes     map[string]string                  // resource → lockID, exclusive holder only
	sharedRes map[string]map[string]struct{}     // resource → set of shared lockIDs
	subs      map[string]map[chan Event]struct{} // resource → subscribers
	counters  map[string]int64                   // persistent atomic counters by key

	// qmu guards the arrival queues. Its own mutex: a contender checks its
	// place without blocking the holders it is waiting for.
	qmu      sync.Mutex
	queues   map[string][]queued
	wakes    map[string]map[chan struct{}]string
	sweepInt time.Duration
	now      func() time.Time
	stopOnce sync.Once
	stop     chan struct{}

	// next is when the sweeper will look next, zero while it waits with no
	// timer; guarded by mu, so a lock added as the sweeper goes idle is seen.
	next      time.Time
	sweepWake chan struct{}
	sweepRuns atomic.Int64
}

type memLock struct {
	ID       string
	Resource string
	Owner    string
	// Site is what the holder is doing, reported to whoever is refused (#1676).
	Site      string
	ExpiresAt time.Time
	Shared    bool
}

// MemoryBackendOption tunes the backend at construction time.
type MemoryBackendOption func(*MemoryBackend)

// WithSweepInterval caps how long the sweeper sleeps while a lock is held, for
// a test whose WithNow clock moves without real time passing.
func WithSweepInterval(d time.Duration) MemoryBackendOption {
	return func(b *MemoryBackend) { b.sweepInt = d }
}

// WithNow overrides the time source. Tests use this to drive deterministic
// expirations without sleeping.
func WithNow(now func() time.Time) MemoryBackendOption {
	return func(b *MemoryBackend) { b.now = now }
}

// NewMemoryBackend returns a fresh in-memory backend and starts its
// background TTL sweeper. Call Close to stop the sweeper and release
// subscriber goroutines.
func NewMemoryBackend(opts ...MemoryBackendOption) *MemoryBackend {
	b := &MemoryBackend{
		locks:     make(map[string]*memLock),
		queues:    make(map[string][]queued),
		wakes:     make(map[string]map[chan struct{}]string),
		byRes:     make(map[string]string),
		sharedRes: make(map[string]map[string]struct{}),
		subs:      make(map[string]map[chan Event]struct{}),
		counters:  make(map[string]int64),
		now:       time.Now,
		stop:      make(chan struct{}),
		sweepWake: make(chan struct{}, 1),
	}
	for _, opt := range opts {
		opt(b)
	}
	go b.sweepLoop()
	return b
}

// Acquire implements Backend. Fails if the resource has an exclusive
// holder OR any shared holder - exclusive is exclusive against everyone.
func (b *MemoryBackend) Acquire(_ context.Context, resource, owner, site, ticket string, ttl time.Duration) (string, Holder, error) {
	if resource == "" || owner == "" {
		return "", Holder{}, fmt.Errorf("locks/memory: resource and owner must be non-empty")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireLocked()
	if _, blocked := b.queuedAhead(resource, ticket); blocked {
		return "", Holder{Site: "queued"}, ErrBusy
	}
	if existing, held := b.byRes[resource]; held {
		return "", Holder{Owner: b.locks[existing].Owner, Site: b.locks[existing].Site}, ErrBusy
	}
	if shared := b.sharedRes[resource]; len(shared) > 0 {
		for id := range shared {
			who := b.locks[id].Owner
			if n := len(shared) - 1; n > 0 {
				who = fmt.Sprintf("%s +%d", who, n)
			}
			return "", Holder{Owner: who, Site: b.locks[id].Site}, ErrBusy
		}
	}
	id, err := randID()
	if err != nil {
		return "", Holder{}, fmt.Errorf("locks/memory: generate id: %w", err)
	}
	b.locks[id] = &memLock{
		ID:        id,
		Resource:  resource,
		Owner:     owner,
		Site:      site,
		ExpiresAt: b.now().Add(ttl),
	}
	b.byRes[resource] = id
	b.sweepByLocked(b.locks[id].ExpiresAt)
	return id, Holder{}, nil
}

// AcquireShared implements Backend. Multiple shared holders may coexist on
// the same resource; only an exclusive holder blocks it.
func (b *MemoryBackend) AcquireShared(_ context.Context, resource, owner, site, ticket string, ttl time.Duration) (string, Holder, error) {
	if resource == "" || owner == "" {
		return "", Holder{}, fmt.Errorf("locks/memory: resource and owner must be non-empty")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireLocked()
	if existing, held := b.byRes[resource]; held {
		return "", Holder{Owner: b.locks[existing].Owner, Site: b.locks[existing].Site}, ErrBusy
	}
	id, err := randID()
	if err != nil {
		return "", Holder{}, fmt.Errorf("locks/memory: generate id: %w", err)
	}
	b.locks[id] = &memLock{
		ID:        id,
		Resource:  resource,
		Owner:     owner,
		Site:      site,
		ExpiresAt: b.now().Add(ttl),
		Shared:    true,
	}
	b.sweepByLocked(b.locks[id].ExpiresAt)
	if b.sharedRes[resource] == nil {
		b.sharedRes[resource] = make(map[string]struct{})
	}
	b.sharedRes[resource][id] = struct{}{}
	return id, Holder{}, nil
}

// Release implements Backend.
func (b *MemoryBackend) Release(_ context.Context, lockID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireLocked()
	l, ok := b.locks[lockID]
	if !ok {
		return ErrNotFound
	}
	delete(b.locks, lockID)
	b.releaseFromIndexLocked(l)
	b.wakeLocked(l.Resource)
	return nil
}

// releaseFromIndexLocked removes l from whichever secondary index
// (byRes or sharedRes) tracks its kind. Caller holds b.mu.
func (b *MemoryBackend) releaseFromIndexLocked(l *memLock) {
	if l.Shared {
		if set, ok := b.sharedRes[l.Resource]; ok {
			delete(set, l.ID)
			if len(set) == 0 {
				delete(b.sharedRes, l.Resource)
			}
		}
		return
	}
	delete(b.byRes, l.Resource)
}

// Renew implements Backend.
func (b *MemoryBackend) Renew(_ context.Context, lockID string, ttl time.Duration) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireLocked()
	l, ok := b.locks[lockID]
	if !ok {
		return ErrExpired
	}
	l.ExpiresAt = b.now().Add(ttl)
	b.sweepByLocked(l.ExpiresAt)
	return nil
}

// PublishAll implements Backend.
func (b *MemoryBackend) PublishAll(ctx context.Context, evs []Event) []error {
	errs := make([]error, len(evs))
	for i, e := range evs {
		errs[i] = b.Publish(ctx, e.Resource, e.Type, e.Payload)
	}
	return errs
}

// Publish implements Backend.
func (b *MemoryBackend) Publish(_ context.Context, resource string, t EventType, payload string) error {
	b.mu.Lock()
	subs := b.subs[resource]
	chans := make([]chan Event, 0, len(subs))
	for ch := range subs {
		chans = append(chans, ch)
	}
	b.mu.Unlock()
	evt := Event{Resource: resource, Type: t, Payload: payload}
	for _, ch := range chans {
		// Non-blocking send - a slow subscriber must not stall publishers.
		// Dropped events are acceptable for IDLE notifications: the client
		// re-syncs from index state on the next user command.
		select {
		case ch <- evt:
		default:
		}
	}
	return nil
}

// Subscribe implements Backend.
func (b *MemoryBackend) Subscribe(_ context.Context, resource string) (<-chan Event, func(), error) {
	ch := make(chan Event, 32)
	b.mu.Lock()
	if b.subs[resource] == nil {
		b.subs[resource] = make(map[chan Event]struct{})
	}
	b.subs[resource][ch] = struct{}{}
	b.mu.Unlock()
	cancel := func() {
		b.mu.Lock()
		if set, ok := b.subs[resource]; ok {
			delete(set, ch)
			if len(set) == 0 {
				delete(b.subs, resource)
			}
		}
		b.mu.Unlock()
		close(ch)
	}
	return ch, cancel, nil
}

// IncrementCounter implements Backend. Auto-initialises missing
// keys to 0 before applying delta. Counter state is independent
// of the lock state and is not touched by the TTL sweeper.
func (b *MemoryBackend) IncrementCounter(_ context.Context, key string, delta int64) (int64, error) {
	if key == "" {
		return 0, fmt.Errorf("locks/memory: counter key must be non-empty")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.counters[key] += delta
	return b.counters[key], nil
}

// Close implements Backend. Idempotent.
func (b *MemoryBackend) Close() error {
	b.stopOnce.Do(func() { close(b.stop) })
	return nil
}

// sweepLoop sleeps until the nearest expiry, and with no lock held it has no
// timer at all: an idle backend never wakes (#2515).
func (b *MemoryBackend) sweepLoop() {
	var timer *time.Timer
	var fire <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-fire:
		case <-b.sweepWake:
		case <-b.stop:
			return
		}
		b.sweepRuns.Add(1)
		b.mu.Lock()
		b.expireLocked()
		d, armed := b.armLocked()
		b.mu.Unlock()
		if timer != nil {
			timer.Stop()
		}
		timer, fire = nil, nil
		if armed {
			timer = time.NewTimer(d)
			fire = timer.C
		}
	}
}

// armLocked sets next to the nearest expiry, capped by sweepInt, and returns
// how long to sleep; false means no lock is held. Caller holds b.mu.
func (b *MemoryBackend) armLocked() (time.Duration, bool) {
	var nearest time.Time
	for _, l := range b.locks {
		if nearest.IsZero() || l.ExpiresAt.Before(nearest) {
			nearest = l.ExpiresAt
		}
	}
	if nearest.IsZero() {
		b.next = time.Time{}
		return 0, false
	}
	now := b.now()
	d := max(nearest.Sub(now), 0)
	if b.sweepInt > 0 && d > b.sweepInt {
		d = b.sweepInt
	}
	b.next = now.Add(d)
	return d, true
}

// sweepByLocked wakes the sweeper when a lock expiring at t is due before it
// would look next. Caller holds b.mu.
func (b *MemoryBackend) sweepByLocked(t time.Time) {
	if !b.next.IsZero() && !t.Before(b.next) {
		return
	}
	b.next = t
	select {
	case b.sweepWake <- struct{}{}:
	default:
	}
}

// expireLocked removes locks whose TTL has elapsed. Caller holds b.mu.
func (b *MemoryBackend) expireLocked() {
	now := b.now()
	for id, l := range b.locks {
		if !now.Before(l.ExpiresAt) {
			delete(b.locks, id)
			b.releaseFromIndexLocked(l)
		}
	}
}

// randID returns a 16-byte hex random identifier.
func randID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
