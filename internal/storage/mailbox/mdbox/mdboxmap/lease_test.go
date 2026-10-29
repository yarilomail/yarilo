package mdboxmap

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/locks"
)

// leaseLocker hands the resource to one holder until it is released or the
// test lapses its lease; a lapsed holder's Renew fails.
type leaseLocker struct {
	mu     sync.Mutex
	holder string
	n      int
}

func (l *leaseLocker) Lock(_ context.Context, resource, owner string, _ time.Duration) (locks.Lock, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holder != "" {
		return locks.Lock{}, locks.ErrBusy
	}
	l.n++
	l.holder = resource + "#" + string(rune('0'+l.n))
	return locks.Lock{ID: l.holder, Resource: resource, Owner: owner}, nil
}
func (l *leaseLocker) LockShared(ctx context.Context, r, o string, ttl time.Duration) (locks.Lock, error) {
	return l.Lock(ctx, r, o, ttl)
}
func (l *leaseLocker) Unlock(_ context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holder == id {
		l.holder = ""
	}
	return nil
}
func (l *leaseLocker) Renew(_ context.Context, id string, _ time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holder != id {
		return locks.ErrExpired
	}
	return nil
}
func (l *leaseLocker) lapse() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.holder = ""
}
func (l *leaseLocker) Subscribe(context.Context, string) (<-chan locks.Event, error) {
	return make(chan locks.Event), nil
}
func (l *leaseLocker) Emit(context.Context, string, locks.EventType, string) error { return nil }
func (l *leaseLocker) HoldsResource(string) (locks.HoldMode, bool)                 { return locks.HoldNone, false }
func (l *leaseLocker) IncrementCounter(context.Context, string, int64) (int64, error) {
	return 0, nil
}
func (l *leaseLocker) Close() error { return nil }

// A placement whose lease lapsed while a peer took the lock must not register
// its record: the peer may already own that end of the file (#2234).
func TestPlacementPastALapsedLeaseIsRefused(t *testing.T) {
	prevTTL, prevRenew := mapLockTTL, mapLockRenew
	mapLockTTL, mapLockRenew = time.Second, time.Millisecond
	t.Cleanup(func() { mapLockTTL, mapLockRenew = prevTTL, prevRenew })

	dir := t.TempDir()
	l := &leaseLocker{}
	first, err := Open(dir, "alice@example.com", WithLocker(l), WithOwner("test.bin/1/alice@example.com/s1"))
	if err != nil {
		t.Fatal(err)
	}
	peer, err := Open(dir, "alice@example.com", WithLocker(l), WithOwner("test.bin/2/alice@example.com/s2"))
	if err != nil {
		t.Fatal(err)
	}

	err = first.Place(func(p *Placement) error {
		l.lapse()
		if _, perr := peer.AppendRecord(1, 0, 10, [16]byte{2}); perr != nil {
			t.Fatalf("peer append: %v", perr)
		}
		select {
		case <-p.ctx.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("lapsed lease was never noticed")
		}
		uid, aerr := p.AppendRecord(1, 0, 10, [16]byte{1})
		if !errors.Is(aerr, context.Canceled) {
			t.Errorf("append past a lapsed lease: uid %d, err %v; want refused", uid, aerr)
		}
		return aerr
	})
	if err == nil {
		t.Fatal("placement past a lapsed lease reported success")
	}
	check, err := Open(dir, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	e, ok, err := check.Lookup(1)
	if err != nil || !ok {
		t.Fatalf("lookup 1: %v %v", ok, err)
	}
	if n := check.st.count(); n != 1 || e.GUID != [16]byte{2} {
		t.Fatalf("%d records, map_uid 1 GUID %x: want only the peer's record", n, e.GUID)
	}
}
