package locks_test

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
)

// muteAfterHandshake answers the version handshake and then never replies, so
// the wait ends at the client's own limit rather than at a server answer.
func muteAfterHandshake(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(c net.Conn) {
				t.Cleanup(func() { c.Close() }) //nolint:errcheck
				rd := bufio.NewReader(c)
				line, rerr := rd.ReadString('\n')
				if rerr != nil || !strings.HasPrefix(line, "VERSION") {
					c.Close() //nolint:errcheck
					return
				}
				_, _ = c.Write([]byte("VERSION\t1\tOK\n"))
				// Read the LOCKWAIT and say nothing.
				_, _ = rd.ReadString('\n')
				select {}
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// A wait that runs out is ErrBusy, one shape for one event: the transport ends
// it as a read timeout or a cut connection depending on the run, and a caller
// classifying on that told contention from an outage by coin flip (#1986).
func TestAWaitThatRunsOutIsBusy(t *testing.T) {
	addr := muteAfterHandshake(t)
	ctx, cancel := context.WithTimeout(locks.WithSite(context.Background(), "test"), 10*time.Second)
	defer cancel()
	client, err := locks.NewClient(ctx, func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close() //nolint:errcheck

	start := time.Now()
	_, err = client.LockWaiting(ctx, "fts:u@test:", locks.Owner("u@test", "waiter"),
		time.Minute, 200*time.Millisecond, false)
	if !errors.Is(err, locks.ErrBusy) {
		t.Fatalf("a wait that reached its limit ended as %v, want ErrBusy", err)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("the wait ended after %v, so it did not reach the limit", elapsed)
	}
}
