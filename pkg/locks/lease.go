package locks

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// UserKey is the lease a pod holds on one account: everything belonging to that
// account is then this pod's to serialise in process (#1840).
func UserKey(user string) string { return "user:" + user }

// leaseTTL bounds a lease so a pod that dies releases the account by expiry.
// Renewed at a third of it, the way every other hold in this package is.
const leaseTTL = 30 * time.Second

// Leased serialises an account's resources in process, under one lease taken
// from the service per account rather than one acquisition per command (#1840).
//
// A cycle through the service costs 46ms end to end, and fifty sessions on one
// folder pay it in turn; the same serialisation in process costs microseconds.
// What the service still provides is the thing that is genuinely between pods:
// only one pod holds the account at a time.
type Leased struct {
	inner Locker
	owner string

	mu     sync.Mutex
	leases map[string]*lease // account -> its lease
	held   map[string]*sync.Mutex
	byID   map[string]*heldLock
}

type lease struct {
	// ready is closed when the lease is taken or has failed: the first caller
	// takes it and the rest wait on its result, not on the lock (#1840).
	ready  chan struct{}
	err    error
	id     string
	refs   int
	cancel context.CancelFunc
	// idle hands the account back a while after the last hold: a stream of
	// commands takes the lease once, not once per command (#1840).
	idle *time.Timer
}

// leaseIdle is how long a lease outlives the last hold on the account.
const leaseIdle = 2 * time.Minute

type heldLock struct {
	resource string
	mu       *sync.Mutex
	account  string
}

// NewLeased wraps a Locker. owner names this pod in the lease it takes.
func NewLeased(inner Locker, owner string) *Leased {
	return &Leased{
		inner:  inner,
		owner:  owner,
		leases: map[string]*lease{},
		held:   map[string]*sync.Mutex{},
		byID:   map[string]*heldLock{},
	}
}

// accountOf reads the account out of a resource key. The keys are built by this
// package, so an unknown shape is a programming error, not input.
func accountOf(resource string) (string, bool) {
	parts := strings.SplitN(resource, ":", 3)
	if len(parts) < 2 {
		return "", false
	}
	switch parts[0] {
	case "mbox", "idx", "mdboxmap", "fts", "mlist", "subs", "user":
		return parts[1], true
	}
	return "", false
}

// Lock takes the account's lease once, then serialises the resource in process.
func (l *Leased) Lock(ctx context.Context, resource, owner string, ttl time.Duration) (Lock, error) {
	account, ok := accountOf(resource)
	if !ok {
		return l.inner.Lock(ctx, resource, owner, ttl)
	}
	if err := l.hold(ctx, account); err != nil {
		return Lock{}, err
	}
	m := l.mutexFor(resource)
	m.Lock()

	id, err := randID()
	if err != nil {
		m.Unlock()
		l.release(account)
		return Lock{}, fmt.Errorf("locks/lease: generate id: %w", err)
	}
	l.mu.Lock()
	l.byID[id] = &heldLock{resource: resource, mu: m, account: account}
	l.mu.Unlock()
	return Lock{ID: id, Resource: resource, Owner: owner, ExpiresAt: time.Now().Add(ttl)}, nil
}

// LockShared is Lock: inside one pod a shared hold and an exclusive one are the
// same mutex, and the account is this pod's for the life of the lease.
func (l *Leased) LockShared(ctx context.Context, resource, owner string, ttl time.Duration) (Lock, error) {
	return l.Lock(ctx, resource, owner, ttl)
}

func (l *Leased) Unlock(ctx context.Context, lockID string) error {
	l.mu.Lock()
	h, ok := l.byID[lockID]
	delete(l.byID, lockID)
	l.mu.Unlock()
	if !ok {
		return l.inner.Unlock(ctx, lockID)
	}
	h.mu.Unlock()
	l.release(h.account)
	return nil
}

func (l *Leased) Renew(ctx context.Context, lockID string, ttl time.Duration) error {
	l.mu.Lock()
	_, ok := l.byID[lockID]
	l.mu.Unlock()
	if ok {
		return nil // an in-process hold does not expire under its holder
	}
	return l.inner.Renew(ctx, lockID, ttl)
}

func (l *Leased) Subscribe(ctx context.Context, resource string) (<-chan Event, error) {
	return l.inner.Subscribe(ctx, resource)
}

func (l *Leased) Emit(ctx context.Context, resource string, t EventType, payload string) error {
	return l.inner.Emit(ctx, resource, t, payload)
}

func (l *Leased) IncrementCounter(ctx context.Context, key string, delta int64) (int64, error) {
	return l.inner.IncrementCounter(ctx, key, delta)
}

func (l *Leased) HoldsResource(resource string) (HoldMode, bool) {
	return l.inner.HoldsResource(resource)
}

func (l *Leased) Close() error { return l.inner.Close() }

func (l *Leased) mutexFor(resource string) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	m, ok := l.held[resource]
	if !ok {
		m = &sync.Mutex{}
		l.held[resource] = m
	}
	return m
}

// hold takes the account's lease, or joins the one this pod already has.
func (l *Leased) hold(ctx context.Context, account string) error {
	l.mu.Lock()
	if ls, ok := l.leases[account]; ok {
		ls.refs++
		if ls.idle != nil {
			ls.idle.Stop()
			ls.idle = nil
		}
		l.mu.Unlock()
		<-ls.ready
		if ls.err != nil {
			l.release(account)
			return ls.err
		}
		return nil
	}
	ls := &lease{ready: make(chan struct{}), refs: 1}
	l.leases[account] = ls
	l.mu.Unlock()

	lk, err := Acquire(WithSite(ctx, "user-lease"), l.inner, UserKey(account), l.owner, leaseTTL)
	if err != nil {
		l.mu.Lock()
		ls.err = fmt.Errorf("locks/lease: take %s: %w", account, err)
		delete(l.leases, account)
		l.mu.Unlock()
		close(ls.ready)
		return ls.err
	}
	renewCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	go l.renew(renewCtx, lk.ID)

	l.mu.Lock()
	ls.id, ls.cancel = lk.ID, cancel
	l.mu.Unlock()
	close(ls.ready)
	return nil
}

// release drops one user of the lease and hands the account back when the last
// one goes: another pod may then take it.
func (l *Leased) release(account string) {
	l.mu.Lock()
	ls, ok := l.leases[account]
	if !ok {
		l.mu.Unlock()
		return
	}
	ls.refs--
	if ls.refs > 0 {
		l.mu.Unlock()
		return
	}
	if ls.idle == nil {
		ls.idle = time.AfterFunc(leaseIdle, func() { l.expire(account) })
	}
	l.mu.Unlock()
}

// expire hands the account back once it has been idle: another pod may take it.
func (l *Leased) expire(account string) {
	l.mu.Lock()
	ls, ok := l.leases[account]
	if !ok || ls.refs > 0 {
		l.mu.Unlock()
		return
	}
	delete(l.leases, account)
	l.mu.Unlock()
	ls.cancel()
	_ = l.inner.Unlock(context.Background(), ls.id)
}

func (l *Leased) renew(ctx context.Context, id string) {
	t := time.NewTicker(leaseTTL / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := l.inner.Renew(ctx, id, leaseTTL); err != nil {
				return
			}
		}
	}
}

var _ Locker = (*Leased)(nil)
