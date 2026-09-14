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

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// counterNow reads a counter without a registry round trip.
func counterNow(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

// twoServerStand runs two servers over one backend, each with a client of its
// own: the shape the chart deploys, where a queue per server is two queues.
func twoServerStand(t *testing.T) (*Client, *Client, *Metrics) {
	t.Helper()
	backend := NewMemoryBackend()
	t.Cleanup(func() { _ = backend.Close() })
	m := NewMetrics(prometheus.NewRegistry(), "test")
	return standOver(t, backend, m), standOver(t, backend, m), m
}

func standOver(t *testing.T, backend Backend, m *Metrics) *Client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(backend, slog.New(slog.NewTextHandler(io.Discard, nil)), m)
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

// Fifty contenders across two servers over one backend, each holding 10ms:
// ordered, the longest wait is the holds ahead of it, not a backoff (#1821).
func TestAContenderWaitsForTheHoldsAheadOfItNotForABackoff(t *testing.T) {
	first, second, metrics := twoServerStand(t)
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
		c := first
		if i%2 == 1 {
			c = second
		}
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

	t.Logf("longest wait: %v", longest)
	// A handover comes from the release announcement. The backstop exists for
	// an announcement that was lost, and a healthy run never reaches it.
	if got := counterNow(t, metrics.waitBackstop); got != 0 {
		t.Errorf("the timer woke %v contenders; every handover should be announced", got)
	}
	ceiling := time.Duration(contenders)*hold + 700*time.Millisecond
	if longest > ceiling {
		t.Errorf("the longest wait was %v, want at most %v (the holds ahead of it)", longest, ceiling)
	}
}

// The order is the backend's, so two servers read one line: a contender that
// joined first stands ahead of one that joined later through the other server.
func TestTheLineIsOneAcrossServers(t *testing.T) {
	backend := NewMemoryBackend()
	defer func() { _ = backend.Close() }()
	q, ok := queueing(backend)
	if !ok {
		t.Fatal("the memory backend carries no queue")
	}
	ctx := context.Background()

	if ahead, err := q.Enqueue(ctx, "r", "first"); err != nil || ahead != 0 {
		t.Fatalf("first joined at %d (err %v), want 0", ahead, err)
	}
	if ahead, err := q.Enqueue(ctx, "r", "second"); err != nil || ahead != 1 {
		t.Fatalf("second joined at %d (err %v), want 1", ahead, err)
	}
	if mine, _ := q.AtFront(ctx, "r", "second"); mine {
		t.Error("the second contender was told it was first")
	}
	if err := q.Dequeue(ctx, "r", "first"); err != nil {
		t.Fatal(err)
	}
	if mine, _ := q.AtFront(ctx, "r", "second"); !mine {
		t.Error("the second contender was not moved up when the first left")
	}
}

// A release reaches a contender that never spoke to the server the holder used.
func TestAReleaseIsAnnouncedToEveryReplica(t *testing.T) {
	backend := NewMemoryBackend()
	defer func() { _ = backend.Close() }()
	q, _ := queueing(backend)
	ctx := context.Background()

	wakes, cancel, err := q.Wakes(ctx, "mailbox/u@x.com/INBOX")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	id, _, err := backend.Acquire(ctx, "mailbox/u@x.com/INBOX", Owner("u@x.com", "s1"), "expunge", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Release(ctx, id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wakes:
	case <-time.After(2 * time.Second):
		t.Error("the release was announced to nobody")
	}
}

// A ticket whose contender died must not hold the line for the ones behind it.
func TestAnAbandonedTicketAgesOutOfTheLine(t *testing.T) {
	clock := time.Now()
	backend := NewMemoryBackend(WithNow(func() time.Time { return clock }))
	defer func() { _ = backend.Close() }()
	q, _ := queueing(backend)
	ctx := context.Background()

	if _, err := q.Enqueue(ctx, "r", "gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue(ctx, "r", "behind"); err != nil {
		t.Fatal(err)
	}
	if mine, _ := q.AtFront(ctx, "r", "behind"); mine {
		t.Fatal("the second contender was at the front while the first stood")
	}
	clock = clock.Add(ticketTTL + time.Second)
	if mine, _ := q.AtFront(ctx, "r", "behind"); !mine {
		t.Error("an abandoned ticket still held the line")
	}
}
