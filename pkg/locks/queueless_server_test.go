package locks

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// serverWithoutQueue answers the waiting LOCK the way a server left behind by a
// rollout does, and grants everything else.
func serverWithoutQueue(t *testing.T) string {
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
				defer c.Close() //nolint:errcheck
				rd := bufio.NewReader(c)
				for {
					line, rerr := rd.ReadString('\n')
					if rerr != nil {
						return
					}
					fields := strings.Split(strings.TrimRight(line, "\n"), "\t")
					switch fields[0] {
					case cmdVersion:
						_, _ = c.Write([]byte("VERSION\t1\tOK\n"))
					case cmdLockWait, cmdLockSharedWait:
						_, _ = c.Write([]byte("ERROR\tunknown_command\n"))
					case cmdLock, cmdLockShared:
						_, _ = c.Write([]byte("OK\tlock-1\n"))
					default:
						_, _ = c.Write([]byte("ERROR\tunknown_command\n"))
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// The bridge is gone: a server that cannot queue is a rollout that did not
// finish, and a client meeting one says so instead of polling behind it (#1823).
func TestAServerThatDoesNotQueueIsAnError(t *testing.T) {
	addr := serverWithoutQueue(t)
	c, err := NewClient(context.Background(), DialTCP(addr))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer c.Close() //nolint:errcheck

	ctx, cancel := context.WithTimeout(WithSite(context.Background(), "write"), 5*time.Second)
	defer cancel()

	_, err = c.LockWaiting(ctx, "res", Owner("u1@test", "sid-1"), time.Minute, time.Second, false)
	if err == nil {
		t.Fatal("LockWaiting succeeded against a server that answers unknown_command")
	}
	if !strings.Contains(err.Error(), "does not queue waiters") {
		t.Errorf("error %v does not say the server cannot queue", err)
	}
	if !strings.Contains(err.Error(), addr) {
		t.Errorf("error %v does not name the server %s", err, addr)
	}

	// Acquire goes through the same answer: it must surface it, not spend the
	// deadline polling the lock the old path used.
	started := time.Now()
	if _, aerr := Acquire(ctx, c, "res", Owner("u1@test", "sid-1"), time.Minute); aerr == nil {
		t.Fatal("Acquire succeeded against a server that answers unknown_command")
	} else if !strings.Contains(aerr.Error(), "does not queue waiters") {
		t.Errorf("Acquire error %v does not say the server cannot queue", aerr)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("Acquire spent %s before failing; it polled instead of failing", elapsed)
	}
}
