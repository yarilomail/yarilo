package locks

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
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
func twoServerStand(t *testing.T) (*Client, *Client, *Metrics, *countingBackend) {
	t.Helper()
	backend := &countingBackend{MemoryBackend: NewMemoryBackend()}
	t.Cleanup(func() { _ = backend.Close() })
	m := NewMetrics(prometheus.NewRegistry(), "test")
	return standOver(t, backend, m), standOver(t, backend, m), m, backend
}

// countingBackend counts what a hand-off costs: every round trip a waiting
// contender makes, whether to read the line or to try the lock.
type countingBackend struct {
	*MemoryBackend
	calls atomic.Int64
}

func (b *countingBackend) AtFront(ctx context.Context, resource, ticket string) (bool, error) {
	b.calls.Add(1)
	return b.MemoryBackend.AtFront(ctx, resource, ticket)
}

func (b *countingBackend) Acquire(ctx context.Context, resource, owner, site, ticket string, ttl time.Duration) (string, Holder, error) {
	b.calls.Add(1)
	return b.MemoryBackend.Acquire(ctx, resource, owner, site, ticket, ttl)
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
	first, second, metrics, backend := twoServerStand(t)
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
		t.Errorf("the timer woke %v contenders that were already first; a handover was owed and never came", got)
	}
	// A hand-off names the next ticket, so a contender whose turn it is not
	// ignores the message without asking the backend anything.
	if got := backend.calls.Load(); got > 2*contenders {
		t.Errorf("%d backend round trips for %d hand-offs; a named hand-off costs one, not one per waiter", got, contenders)
	}
	t.Logf("backend round trips: %d", backend.calls.Load())
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

	// Queued, so the release names this ticket and the subscription passes it.
	if _, eerr := q.Enqueue(ctx, "mailbox/u@x.com/INBOX", "waiter"); eerr != nil {
		t.Fatal(eerr)
	}
	wakes, cancel, err := q.Wakes(ctx, "mailbox/u@x.com/INBOX", "waiter")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	id, _, err := backend.Acquire(ctx, "mailbox/u@x.com/INBOX", Owner("u@x.com", "s1"), "expunge", "waiter", time.Minute)
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

// A hold that ends by TTL announces nothing, so the next contender waits for
// its timer. Counted apart from a hand-off that was lost (#1809).
func TestALockThatExpiredIsCountedApartFromALostHandOff(t *testing.T) {
	backend := NewMemoryBackend(WithSweepInterval(5 * time.Millisecond))
	t.Cleanup(func() { _ = backend.Close() })
	m := NewMetrics(prometheus.NewRegistry(), "test")
	client := standOver(t, backend, m)

	ctx := WithSite(context.Background(), "expunge")
	lk, err := Acquire(ctx, client, "mailbox/u@x.com/INBOX", Owner("u@x.com", "s1"), 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	before := counterNow(t, m.expiredUnrel)

	// Past the TTL, so the release finds nothing to release.
	time.Sleep(200 * time.Millisecond)
	if uerr := client.Unlock(ctx, lk.ID); uerr == nil {
		t.Fatal("unlocking an expired lock reported success")
	}
	if got := counterNow(t, m.expiredUnrel) - before; got != 1 {
		t.Errorf("an expired hold was counted %v times, want 1", got)
	}
}

// Other tickets' turns may be dropped without limit; this one's never is, or
// the contender at the head waits out its deadline on the timer (#1809).
func TestAHandOffSurvivesABurstOfOtherTurns(t *testing.T) {
	backend := NewMemoryBackend()
	t.Cleanup(func() { _ = backend.Close() })
	m := NewMetrics(prometheus.NewRegistry(), "test")
	client := standOver(t, backend, m)

	const resource = "mailbox/u@x.com/INBOX"
	ctx := WithSite(context.Background(), "expunge")

	// The lock is held, so the contender queues at the head and waits.
	held, err := Acquire(ctx, client, resource, Owner("u@x.com", "holder"), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	got := make(chan error, 1)
	go func() {
		lk, aerr := Acquire(ctx, client, resource, Owner("u@x.com", "waiter"), 30*time.Second)
		if aerr == nil {
			_ = client.Unlock(ctx, lk.ID)
		}
		got <- aerr
	}()

	// Turns for tickets nobody is waiting on, far more than any buffer holds.
	q, _ := queueing(backend)
	for i := 0; i < 500; i++ {
		if _, eerr := q.Enqueue(ctx, resource, fmt.Sprintf("ghost%d", i)); eerr != nil {
			t.Fatal(eerr)
		}
		if derr := q.Dequeue(ctx, resource, fmt.Sprintf("ghost%d", i)); derr != nil {
			t.Fatal(derr)
		}
	}

	backstopBefore := counterNow(t, m.waitBackstop)
	if uerr := client.Unlock(ctx, held.ID); uerr != nil {
		t.Fatal(uerr)
	}
	select {
	case aerr := <-got:
		if aerr != nil {
			t.Fatalf("the contender at the head did not get its turn: %v", aerr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the contender at the head never got its turn")
	}
	if n := counterNow(t, m.waitBackstop) - backstopBefore; n != 0 {
		t.Errorf("the hand-off came from the timer %v times; it was announced", n)
	}
}

// The queue is a right, not a notification: only its head takes the lock, even
// when the resource is free (#1809).
func TestOnlyTheHeadOfTheQueueMayTakeTheLock(t *testing.T) {
	backend := NewMemoryBackend()
	t.Cleanup(func() { _ = backend.Close() })
	q, _ := queueing(backend)
	ctx := context.Background()

	const resource = "mbox:u@x.com:INBOX"
	if _, err := q.Enqueue(ctx, resource, "head"); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue(ctx, resource, "behind"); err != nil {
		t.Fatal(err)
	}

	// The resource is free, and the one behind tries first.
	if _, _, err := backend.Acquire(ctx, resource, Owner("u@x.com", "s2"), "expunge", "behind", time.Minute); err == nil {
		t.Fatal("a contender that is not at the head took a free lock")
	}
	// A caller with no ticket is refused for the same reason.
	if _, _, err := backend.Acquire(ctx, resource, Owner("u@x.com", "s3"), "expunge", "", time.Minute); err == nil {
		t.Fatal("a caller with no ticket took a lock someone was queued for")
	}
	id, _, err := backend.Acquire(ctx, resource, Owner("u@x.com", "s1"), "expunge", "head", time.Minute)
	if err != nil {
		t.Fatalf("the head was refused its own lock: %v", err)
	}
	if err := backend.Release(ctx, id); err != nil {
		t.Fatal(err)
	}
}
