package locks

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// dropOnUnlock is a lock service that grants every shared lock and drops the
// connection on every UNLOCK, as a pod going away mid-release does.
func dropOnUnlock(t *testing.T) string {
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
					switch {
					case strings.HasPrefix(line, cmdVersion):
						_ = writeFields(conn, cmdVersion, protocolVersion, respOK, "g1")
					case strings.HasPrefix(line, cmdLockShared):
						_ = writeFields(conn, respOK, "id-1")
					default:
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// A release that fails in an outage still gives the hold up on the client: the
// session's next exclusive request on the resource is not refused as an
// upgrade of a shared hold the server no longer has (#2509).
func TestAFailedReleaseForgetsTheHold(t *testing.T) {
	tests := []struct {
		name  string
		stand func(t *testing.T) (*Client, func())
	}{
		{
			name: "the lock store is down",
			stand: func(t *testing.T) (*Client, func()) {
				backend := &outageBackend{MemoryBackend: NewMemoryBackend()}
				t.Cleanup(func() { _ = backend.Close() })
				return standOver(t, backend, NewMetrics(prometheus.NewRegistry(), "test")), func() { backend.down.Store(true) }
			},
		},
		{
			name: "the connection drops on release",
			stand: func(t *testing.T) (*Client, func()) {
				c, err := NewClient(context.Background(), DialTCP(dropOnUnlock(t)))
				if err != nil {
					t.Fatalf("client: %v", err)
				}
				t.Cleanup(func() { _ = c.Close() })
				return c, func() {}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, outage := tc.stand(t)
			ctx := WithSite(t.Context(), "test")
			held, err := c.LockShared(ctx, "mbox:u@x.com:INBOX", Owner("u@x.com", "s"), time.Minute)
			if err != nil {
				t.Fatalf("lock shared: %v", err)
			}
			outage()
			if err := c.Unlock(ctx, held.ID); err == nil {
				t.Fatal("the release succeeded, so this tests nothing")
			}
			if mode, ok := c.HoldsResource("mbox:u@x.com:INBOX"); ok {
				t.Errorf("after a failed release the client still holds the resource %s", mode)
			}
			if _, err := Reentrant(c, "mbox:u@x.com:INBOX", "expunge", false); errors.Is(err, ErrHoldNotExclusive) {
				t.Errorf("the next exclusive request was refused: %v", err)
			}
		})
	}
}
