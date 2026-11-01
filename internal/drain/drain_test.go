package drain_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/auth/protocol"
	"github.com/yarilomail/yarilo/v2/internal/drain"
	"github.com/yarilomail/yarilo/v2/internal/warden"
)

const grace = 3 * time.Second

// servers are the two processes that slept the whole grace on SIGTERM (#2433).
var servers = map[string]func(context.Context, net.Listener) error{
	"auth":   protocol.NewServer(nil).Serve,
	"warden": warden.NewServer(1).Serve,
}

// serve starts one server and returns its cancel, its WaitGroup and its address.
func serve(t *testing.T, run func(context.Context, net.Listener) error) (context.CancelFunc, *sync.WaitGroup, string) {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var serving sync.WaitGroup
	serving.Add(1)
	go func() {
		defer serving.Done()
		run(ctx, ln) //nolint:errcheck
	}()
	return cancel, &serving, ln.Addr().String()
}

// Without a connection the shutdown is over at once, not after the grace.
func TestWithoutConnectionsTheShutdownDoesNotWait(t *testing.T) {
	for name, run := range servers {
		cancel, serving, _ := serve(t, run)
		start := time.Now()
		cancel()
		if !drain.Wait(serving, grace) {
			t.Errorf("%s: the grace ran out with no connection open", name)
		}
		if took := time.Since(start); took > 500*time.Millisecond {
			t.Errorf("%s: stopping with no connection took %v", name, took)
		}
	}
}

// An open connection is waited for, and the shutdown ends when it closes,
// well inside the grace.
func TestTheShutdownWaitsForAnOpenConnection(t *testing.T) {
	const open = 300 * time.Millisecond
	for name, run := range servers {
		cancel, serving, addr := serve(t, run)
		conn, err := new(net.Dialer).DialContext(t.Context(), "tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond) // the server has taken the connection
		time.AfterFunc(open, func() { conn.Close() })
		start := time.Now()
		cancel()
		if !drain.Wait(serving, grace) {
			t.Errorf("%s: the grace ran out though the connection closed", name)
		}
		if took := time.Since(start); took < open-50*time.Millisecond || took > open+time.Second {
			t.Errorf("%s: stopping took %v; want about %v, when the connection closed", name, took, open)
		}
	}
}

// A connection that stays open past the grace does not hold the process.
func TestTheGraceBoundsTheWait(t *testing.T) {
	cancel, serving, addr := serve(t, servers["auth"])
	conn, err := new(net.Dialer).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	time.Sleep(50 * time.Millisecond)
	short := 200 * time.Millisecond
	start := time.Now()
	cancel()
	if drain.Wait(serving, short) {
		t.Error("the connection is still open, yet the servers reported returned")
	}
	if took := time.Since(start); took > short+500*time.Millisecond {
		t.Errorf("the wait outlived its grace: %v for %v", took, short)
	}
}

func TestAZeroGraceDoesNotWait(t *testing.T) {
	var serving sync.WaitGroup
	serving.Add(1)
	defer serving.Done()
	start := time.Now()
	if drain.Wait(&serving, 0) {
		t.Error("a server still running was reported returned")
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Errorf("a zero grace waited %v", took)
	}
}
