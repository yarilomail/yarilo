package locks

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var errStoreDown = errors.New("store: connection refused")

// outageBackend is a store that can be cut off under a live server.
type outageBackend struct {
	*MemoryBackend
	down atomic.Bool
}

func (b *outageBackend) Acquire(ctx context.Context, resource, owner, site, ticket string, ttl time.Duration) (string, Holder, error) {
	if b.down.Load() {
		return "", Holder{}, errStoreDown
	}
	return b.MemoryBackend.Acquire(ctx, resource, owner, site, ticket, ttl)
}

func (b *outageBackend) AcquireShared(ctx context.Context, resource, owner, site, ticket string, ttl time.Duration) (string, Holder, error) {
	if b.down.Load() {
		return "", Holder{}, errStoreDown
	}
	return b.MemoryBackend.AcquireShared(ctx, resource, owner, site, ticket, ttl)
}

func (b *outageBackend) Release(ctx context.Context, lockID string) error {
	if b.down.Load() {
		return errStoreDown
	}
	return b.MemoryBackend.Release(ctx, lockID)
}

func (b *outageBackend) Renew(ctx context.Context, lockID string, ttl time.Duration) error {
	if b.down.Load() {
		return errStoreDown
	}
	return b.MemoryBackend.Renew(ctx, lockID, ttl)
}

func (b *outageBackend) IncrementCounter(ctx context.Context, key string, delta int64) (int64, error) {
	if b.down.Load() {
		return 0, errStoreDown
	}
	return b.MemoryBackend.IncrementCounter(ctx, key, delta)
}

// A lock service that is up but cannot reach its store can hold no state: to
// the client that is the service being unavailable, the same as a dropped
// connection, so the protocol above answers "try again" and not "this server
// is broken" (#2503).
func TestAStoreOutageReachesTheClientAsUnavailable(t *testing.T) {
	backend := &outageBackend{MemoryBackend: NewMemoryBackend()}
	t.Cleanup(func() { _ = backend.Close() })
	c := standOver(t, backend, NewMetrics(prometheus.NewRegistry(), "test"))
	ctx := WithSite(t.Context(), "test")
	owner := Owner("u@x.com", "s")

	held, err := c.Lock(ctx, "r/held", owner, time.Minute)
	if err != nil {
		t.Fatalf("lock while the store is up: %v", err)
	}
	backend.down.Store(true)

	calls := map[string]func() error{
		"lock":        func() error { _, err := c.Lock(ctx, "r/a", owner, time.Minute); return err },
		"lock shared": func() error { _, err := c.LockShared(ctx, "r/b", owner, time.Minute); return err },
		"unlock":      func() error { return c.Unlock(ctx, held.ID) },
		"renew":       func() error { return c.Renew(ctx, held.ID, time.Minute) },
		"counter":     func() error { _, err := c.IncrementCounter(ctx, "k", 1); return err },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s with the store down = %v, want ErrUnavailable", name, err)
		}
	}
}

// Only the store failure is an outage: a request the server refuses keeps
// being an error of its own.
func TestOnlyAnInternalReplyIsUnavailable(t *testing.T) {
	for reason, want := range map[string]bool{"internal": true, "bad_ttl": false, "unknown_command": false} {
		err := serverError("locks/client: server error", []string{respError, reason})
		if errors.Is(err, ErrUnavailable) != want {
			t.Errorf("ERROR %s: unavailable = %v, want %v (%v)", reason, !want, want, err)
		}
		if !strings.Contains(err.Error(), reason) {
			t.Errorf("ERROR %s: the reason was lost: %v", reason, err)
		}
	}
}

// dropOnWait is a lock service that completes the handshake and then drops
// every waiting request without an answer, as a pod does when it goes away.
func dropOnWait(t *testing.T) string {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				r := bufio.NewReader(conn)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if strings.HasPrefix(line, cmdVersion) {
						_ = writeFields(conn, cmdVersion, protocolVersion, respOK, "g1")
						continue
					}
					return
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// A waiting request whose connection drops is the lock service gone away, as
// on the control pool; a waiter the caller gave up on stays a cancellation.
func TestADroppedWaitIsUnavailableAndACancelledOneIsNot(t *testing.T) {
	c, err := NewClient(context.Background(), DialTCP(dropOnWait(t)))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	owner := Owner("u@x.com", "s")

	if _, err := c.LockWaiting(WithSite(t.Context(), "test"), "r", owner, time.Minute, time.Second, false); !errors.Is(err, ErrUnavailable) {
		t.Errorf("a dropped wait = %v, want ErrUnavailable", err)
	}

	ctx, cancel := context.WithCancel(WithSite(t.Context(), "test"))
	cancel()
	if _, err := c.LockWaiting(ctx, "r", owner, time.Minute, time.Second, false); errors.Is(err, ErrUnavailable) {
		t.Errorf("a cancelled wait = %v, want it not called an outage", err)
	}
}
