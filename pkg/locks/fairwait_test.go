package locks

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"
)

// waitStand is a server over a memory backend on a loopback listener, with a
// client of its own.
func waitStand(t *testing.T) *Client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(NewMemoryBackend(), slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		srv.Close()
		<-done
	})
	c, err := NewClient(context.Background(), DialTCP(ln.Addr().String()))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// Fifty contenders, each holding for 10ms. Queued, the longest wait is bounded
// by the holds ahead of it; polling, it is bounded by the backoff step and the
// draw a contender can keep losing (#1821).
func TestAContenderWaitsForTheHoldsAheadOfItNotForABackoff(t *testing.T) {
	c := waitStand(t)
	const (
		contenders = 50
		hold       = 10 * time.Millisecond
	)

	ctx, cancel := context.WithTimeout(WithSite(context.Background(), "expunge"), 60*time.Second)
	defer cancel()

	var mu sync.Mutex
	var longest time.Duration
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started := time.Now()
			lk, err := Acquire(ctx, c, "mailbox/u@x.com/INBOX", Owner("u@x.com", fmt.Sprintf("s%d", i)), 30*time.Second)
			waited := time.Since(started)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			mu.Lock()
			if waited > longest {
				longest = waited
			}
			mu.Unlock()
			time.Sleep(hold)
			if uerr := c.Unlock(ctx, lk.ID); uerr != nil {
				t.Errorf("unlock: %v", uerr)
			}
		}()
	}
	wg.Wait()

	// The holds ahead of the last contender, plus the round trip each handover
	// costs. Measured: 0.55s queued, 2.18s polling.
	ceiling := time.Duration(contenders)*hold + 700*time.Millisecond
	if longest > ceiling {
		t.Errorf("the longest wait was %v, want at most %v (the holds ahead of it)", longest, ceiling)
	}
}

// A contender that gives up must not take the line with it: the one behind it
// is woken, not left until its own deadline.
func TestAbandoningTheFrontHandsTheTurnOn(t *testing.T) {
	q := newWaitQueue()
	first, _ := q.join("r")
	second, depth := q.join("r")
	if depth != 1 {
		t.Fatalf("the second contender joined at depth %d, want 1", depth)
	}
	select {
	case <-second.ready:
		t.Fatal("the second contender was told to try while the first still stood")
	default:
	}
	q.leave("r", first)
	select {
	case <-second.ready:
	default:
		t.Error("the second contender was not woken when the first left")
	}
}

// An unlock wakes the front of the line, and only it: waking every contender
// is the lottery this replaces.
func TestAReleaseWakesTheFrontOfTheLine(t *testing.T) {
	q := newWaitQueue()
	front, _ := q.join("r")
	<-front.ready // the first joiner may try at once
	behind, _ := q.join("r")

	q.wake("r")
	select {
	case <-front.ready:
	default:
		t.Error("the front was not woken by the release")
	}
	select {
	case <-behind.ready:
		t.Error("a contender behind the front was woken too")
	default:
	}
	if got := q.depth("r"); got != 2 {
		t.Errorf("depth = %d, want 2", got)
	}
}
