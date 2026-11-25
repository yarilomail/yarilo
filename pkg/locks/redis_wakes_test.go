package locks

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
)

// storeCounter counts what a replica asks of the store: new connections and
// commands by name.
type storeCounter struct {
	dials atomic.Int64
	mu    sync.Mutex
	cmds  map[string]int
}

func (c *storeCounter) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c.dials.Add(1)
		return next(ctx, network, addr)
	}
}

func (c *storeCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		c.mu.Lock()
		c.cmds[cmd.Name()]++
		c.mu.Unlock()
		return next(ctx, cmd)
	}
}

func (c *storeCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		c.mu.Lock()
		c.cmds["pipeline"]++
		c.mu.Unlock()
		return next(ctx, cmds)
	}
}

func (c *storeCounter) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.cmds))
	for k, v := range c.cmds {
		out[k] = v
	}
	return out
}

func (c *storeCounter) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.cmds {
		n += v
	}
	return n
}

type redisReplica struct {
	backend *RedisBackend
	client  *Client
	addr    string
	metrics *Metrics
	store   *storeCounter
}

// redisReplicas runs n lock services over one store, each with a client of
// its own to the store and a client of its own to the service: the shape the
// chart deploys.
func redisReplicas(t *testing.T, mr *miniredis.Miniredis, n int) []redisReplica {
	t.Helper()
	out := make([]redisReplica, n)
	for i := range out {
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		sc := &storeCounter{cmds: map[string]int{}}
		rdb.AddHook(sc)
		b := NewRedisBackend(rdb)
		b.RegisterMetrics(prometheus.NewRegistry())
		if err := b.ListenWakes(t.Context()); err != nil {
			t.Fatalf("listen for releases: %v", err)
		}
		t.Cleanup(func() { _ = b.Close() })
		m := NewMetrics(prometheus.NewRegistry(), "test")
		addr, c := standAddrWith(t, b, m)
		out[i] = redisReplica{backend: b, client: c, addr: addr, metrics: m, store: sc}
	}
	return out
}

func standAddrWith(t *testing.T, backend Backend, m *Metrics) (string, *Client) {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(backend, testLogger(), m)
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
	return ln.Addr().String(), c
}

const wakeResource = "mailbox/u@x.com/INBOX"

func waitCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(WithSite(t.Context(), "expunge"), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// inLine waits until n tickets stand in the resource's line.
func inLine(t *testing.T, mr *miniredis.Miniredis, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if members, _ := mr.ZMembers("yarilo:locks:q:" + wakeResource); len(members) == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the line did not reach %d tickets", n)
}

// A release through one replica wakes the contender waiting on the other at
// once, through the shared subscription, not by the backstop.
func TestAWaiterOnOneReplicaIsWokenByAReleaseThroughTheOther(t *testing.T) {
	mr := miniredis.RunT(t)
	rs := redisReplicas(t, mr, 2)
	ctx := waitCtx(t)

	held, err := Acquire(ctx, rs[0].client, wakeResource, Owner("u@x.com", "holder"), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		after time.Duration
		err   error
	}
	got := make(chan result, 1)
	var released time.Time
	var relMu sync.Mutex
	go func() {
		lk, aerr := Acquire(ctx, rs[1].client, wakeResource, Owner("u@x.com", "waiter"), 30*time.Second)
		relMu.Lock()
		after := time.Since(released)
		relMu.Unlock()
		if aerr == nil {
			_ = rs[1].client.Unlock(ctx, lk.ID)
		}
		got <- result{after, aerr}
	}()
	inLine(t, mr, 1)
	relMu.Lock()
	released = time.Now()
	relMu.Unlock()
	if err := rs[0].client.Unlock(ctx, held.ID); err != nil {
		t.Fatal(err)
	}
	r := <-got
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.after >= lostWakeBackstop {
		t.Errorf("the waiter took the lock %v after the release; the backstop, not the release, woke it", r.after)
	}
	if n := counterNow(t, rs[1].metrics.waitBackstop); n != 0 {
		t.Errorf("the backstop woke %v contenders", n)
	}
}

// Three contenders joining alternately through two replicas get the lock in
// the order they joined (#1821).
func TestTheLineKeepsItsOrderAcrossReplicas(t *testing.T) {
	mr := miniredis.RunT(t)
	rs := redisReplicas(t, mr, 2)
	ctx := waitCtx(t)

	held, err := Acquire(ctx, rs[0].client, wakeResource, Owner("u@x.com", "holder"), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	order := make(chan int, 3)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		r := rs[i%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			lk, aerr := Acquire(ctx, r.client, wakeResource, Owner("u@x.com", fmt.Sprintf("s%d", i)), 30*time.Second)
			if aerr != nil {
				t.Errorf("contender %d: %v", i, aerr)
				return
			}
			order <- i
			time.Sleep(10 * time.Millisecond)
			_ = r.client.Unlock(ctx, lk.ID)
		}()
		inLine(t, mr, i+1)
		time.Sleep(2 * time.Millisecond) // the line is ordered by the millisecond
	}
	if err := rs[0].client.Unlock(ctx, held.ID); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(order)
	want := 0
	for got := range order {
		if got != want {
			t.Fatalf("contender %d took the lock in place %d", got, want)
		}
		want++
	}
}

// A contender that left the line takes its ticket with it, and the one behind
// it is woken by the release rather than waiting out the ticket.
func TestAWaiterThatLeftHandsItsTurnOn(t *testing.T) {
	mr := miniredis.RunT(t)
	rs := redisReplicas(t, mr, 2)
	ctx := waitCtx(t)

	held, err := Acquire(ctx, rs[0].client, wakeResource, Owner("u@x.com", "holder"), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	joinAndVanish(t, rs[1].addr, wakeResource)
	inLine(t, mr, 0)

	got := make(chan time.Duration, 1)
	go func() {
		started := time.Now()
		lk, aerr := Acquire(ctx, rs[1].client, wakeResource, Owner("u@x.com", "next"), 30*time.Second)
		if aerr != nil {
			t.Error(aerr)
		} else {
			_ = rs[1].client.Unlock(ctx, lk.ID)
		}
		got <- time.Since(started)
	}()
	inLine(t, mr, 1)
	if err := rs[0].client.Unlock(ctx, held.ID); err != nil {
		t.Fatal(err)
	}
	if waited := <-got; waited > 5*time.Second {
		t.Errorf("the contender behind a vanished one waited %v", waited)
	}
}

// A broken subscription may have dropped a release, so when it is back every
// waiter of the replica is woken to look for itself, and the subscription
// carries releases again.
func TestABrokenSubscriptionWakesEveryWaiterWhenItIsBack(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	b := NewRedisBackend(rdb)
	t.Cleanup(func() { _ = b.Close() })
	ctx := t.Context()

	wakes, stop, err := b.Wakes(ctx, wakeResource, "t1")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	mr.Close()
	if err := mr.Restart(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wakes:
	case <-time.After(10 * time.Second):
		t.Fatal("the waiter was not woken when the subscription came back")
	}
	if n := counterNow(t, b.wakes.resubscribed); n != 1 {
		t.Errorf("resubscribed = %v, want 1", n)
	}

	if err := rdb.Publish(ctx, b.wakeChannel(wakeResource), "t1").Err(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wakes:
	case <-time.After(2 * time.Second):
		t.Fatal("a release after the subscription came back reached nobody")
	}
}

// A broken subscription does not cost the waiter its lock: the holder's
// release while the replica was cut off is found when it is back.
func TestAWaiterTakesTheLockAcrossABrokenSubscription(t *testing.T) {
	mr := miniredis.RunT(t)
	rs := redisReplicas(t, mr, 2)
	ctx := waitCtx(t)

	held, err := Acquire(ctx, rs[0].client, wakeResource, Owner("u@x.com", "holder"), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		lk, aerr := Acquire(ctx, rs[1].client, wakeResource, Owner("u@x.com", "waiter"), 30*time.Second)
		if aerr == nil {
			_ = rs[1].client.Unlock(ctx, lk.ID)
		}
		got <- aerr
	}()
	inLine(t, mr, 1)
	mr.Close()
	if err := mr.Restart(); err != nil {
		t.Fatal(err)
	}
	if err := rs[0].client.Unlock(ctx, held.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the waiter never took the lock")
	}
}

// Waiting opens no connection to the store: the replica's one subscription
// carries every wake. Before, each wait dialled and greeted the store.
func TestAWaitOpensNoConnectionToTheStore(t *testing.T) {
	mr := miniredis.RunT(t)
	rs := redisReplicas(t, mr, 2)
	ctx := waitCtx(t)

	round := func(i int) {
		held, err := Acquire(ctx, rs[0].client, wakeResource, Owner("u@x.com", "holder"), 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			lk, aerr := Acquire(ctx, rs[1].client, wakeResource, Owner("u@x.com", fmt.Sprintf("w%d", i)), 30*time.Second)
			if aerr == nil {
				aerr = rs[1].client.Unlock(ctx, lk.ID)
			}
			done <- aerr
		}()
		inLine(t, mr, 1)
		if err := rs[0].client.Unlock(ctx, held.ID); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ { // the pools fill
		round(i)
	}
	before := rs[0].store.dials.Load() + rs[1].store.dials.Load()
	const waits = 20
	for i := 0; i < waits; i++ {
		round(100 + i)
	}
	// The command pool may still grow by a connection or two while the
	// rounds overlap; a connection per wait is at least one each.
	if d := rs[0].store.dials.Load() + rs[1].store.dials.Load() - before; d >= waits/2 {
		t.Errorf("%d waits opened %d connections to the store; a wait must open none", waits, d)
	}
}

// freeLockCommands is what one acquire and one release of a free lock ask of
// the store: join the line, try, leave the line, release, and announce the
// release. Pinned, so a change that adds a round trip is seen.
const freeLockCommands = 5

func TestAFreeLockCostsAKnownNumberOfStoreCommands(t *testing.T) {
	mr := miniredis.RunT(t)
	rs := redisReplicas(t, mr, 1)
	ctx := waitCtx(t)

	// The first acquire loads the scripts; the count is of the ones after.
	lk, err := Acquire(ctx, rs[0].client, wakeResource, Owner("u@x.com", "warm"), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := rs[0].client.Unlock(ctx, lk.ID); err != nil {
		t.Fatal(err)
	}
	before := rs[0].store.total()
	was := rs[0].store.snapshot()
	lk, err = Acquire(ctx, rs[0].client, wakeResource, Owner("u@x.com", "s1"), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := rs[0].client.Unlock(ctx, lk.ID); err != nil {
		t.Fatal(err)
	}
	got := rs[0].store.total() - before
	diff := map[string]int{}
	for k, v := range rs[0].store.snapshot() {
		if d := v - was[k]; d != 0 {
			diff[k] = d
		}
	}
	t.Logf("store commands of one free lock: %v", diff)
	if got != freeLockCommands {
		t.Errorf("a free lock took %d store commands, want %d", got, freeLockCommands)
	}
}
