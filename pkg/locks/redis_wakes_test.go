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
// the store: join the line and take the lock in one script, release and
// announce in another. Pinned, so a change that adds a round trip is seen.
const freeLockCommands = 2

// waitedLockCommands is what a contender that had to wait asks of the store:
// join and find it held, take it when woken, release.
const waitedLockCommands = 3

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

func commandsSince(sc *storeCounter, was map[string]int) (int, map[string]int) {
	diff := map[string]int{}
	n := 0
	for k, v := range sc.snapshot() {
		if d := v - was[k]; d != 0 {
			diff[k] = d
			n += d
		}
	}
	return n, diff
}

// warmResource is where a test loads the scripts before counting: a release
// on it reaches nobody waiting on wakeResource (#2735).
const warmResource = "mailbox/u@x.com/Warm"

// inLineOn waits until resource's line holds n tickets.
func inLineOn(t *testing.T, mr *miniredis.Miniredis, resource string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if members, _ := mr.ZMembers("yarilo:locks:q:" + resource); len(members) == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the line of %s did not reach %d tickets", resource, n)
}

// logWakes prints every wake published for resource, as it arrives: the
// ticket it names, or that it names none and so reaches every waiter. Read
// through a client of its own, so it adds nothing to the counts.
func logWakes(t *testing.T, mr *miniredis.Miniredis, b *RedisBackend, resource string) {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ps := rdb.Subscribe(t.Context(), b.wakeChannel(resource))
	if _, err := ps.Receive(t.Context()); err != nil {
		t.Fatalf("subscribe to the wakes: %v", err)
	}
	start := time.Now()
	exited := make(chan struct{})
	// The printer is gone before the test ends: a log after it would panic.
	t.Cleanup(func() { _ = ps.Close(); <-exited })
	go func() {
		defer close(exited)
		for msg := range ps.Channel() {
			woke := "ticket " + msg.Payload
			if msg.Payload == "" {
				woke = "no ticket: every waiter tries"
			}
			t.Logf("+%v wake on %s: %s", time.Since(start).Round(time.Millisecond), resource, woke)
		}
	}()
}

// waitedOnce has a contender on rs[1] wait for a lock rs[0] holds on resource,
// then take it and release it. between runs while it waits in line.
func waitedOnce(t *testing.T, mr *miniredis.Miniredis, rs []redisReplica, resource, name string, between func()) {
	t.Helper()
	ctx := waitCtx(t)
	held, err := Acquire(ctx, rs[0].client, resource, Owner("u@x.com", "holder"), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		lk, aerr := Acquire(ctx, rs[1].client, resource, Owner("u@x.com", name), 30*time.Second)
		if aerr == nil {
			aerr = rs[1].client.Unlock(ctx, lk.ID)
		}
		done <- aerr
	}()
	inLineOn(t, mr, resource, 1)
	if between != nil {
		between()
	}
	if err := rs[0].client.Unlock(ctx, held.ID); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// The scripts are loaded on another resource than the one counted: the warm
// round ends with a release on an empty line, which names no ticket and so
// wakes every waiter of its resource. Late on a slow runner, on the counted
// resource it woke the counted waiter into a fourth command (#2735).
func TestAWaitedLockCostsAKnownNumberOfStoreCommands(t *testing.T) {
	mr := miniredis.RunT(t)
	rs := redisReplicas(t, mr, 2)
	logWakes(t, mr, rs[1].backend, wakeResource)

	waitedOnce(t, mr, rs, warmResource, "warm", nil) // loads the scripts
	was := rs[1].store.snapshot()
	waitedOnce(t, mr, rs, wakeResource, "waiter", nil)
	got, diff := commandsSince(rs[1].store, was)
	t.Logf("store commands of one waited lock: %v", diff)
	if got != waitedLockCommands {
		t.Errorf("a waited lock took %d store commands, want %d", got, waitedLockCommands)
	}
}

// A release that names no ticket reaches every waiter of the resource on
// purpose: one subscribed but not yet in line must hear it. A waiter in line
// takes it for its turn, tries once, and is refused: exactly one command more
// than a waited lock, and no backstop (#2735).
func TestAReleaseNamingNoTicketCostsAWaiterOneTry(t *testing.T) {
	mr := miniredis.RunT(t)
	rs := redisReplicas(t, mr, 2)
	logWakes(t, mr, rs[1].backend, wakeResource)
	pub := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = pub.Close() })

	waitedOnce(t, mr, rs, warmResource, "warm", nil)
	was := rs[1].store.snapshot()
	bs := counterNow(t, rs[1].metrics.waitBackstop)
	waitedOnce(t, mr, rs, wakeResource, "waiter", func() {
		tried := rs[1].store.snapshot()["evalsha"]
		if err := pub.Publish(t.Context(), rs[1].backend.wakeChannel(wakeResource), "").Err(); err != nil {
			t.Fatal(err)
		}
		// The release waits for the extra try, so the count cannot race it.
		deadline := time.Now().Add(5 * time.Second)
		for rs[1].store.snapshot()["evalsha"] == tried {
			if time.Now().After(deadline) {
				t.Fatal("the waiter did not try on a release naming no ticket")
			}
			time.Sleep(time.Millisecond)
		}
	})
	got, diff := commandsSince(rs[1].store, was)
	t.Logf("store commands of one waited lock with a release naming no ticket: %v", diff)
	if got != waitedLockCommands+1 || diff["evalsha"] != waitedLockCommands+1 {
		t.Errorf("took %v, want evalsha:%d and nothing else", diff, waitedLockCommands+1)
	}
	if n := counterNow(t, rs[1].metrics.waitBackstop) - bs; n != 0 {
		t.Errorf("the backstop woke the waiter %v times, want 0", n)
	}
}

// Readers queued behind a writer share the lock once it goes: the first
// reader's grant hands the turn to the next at once, not at its release.
func TestQueuedReadersShareTheLockWhenTheWriterGoes(t *testing.T) {
	mr := miniredis.RunT(t)
	rs := redisReplicas(t, mr, 2)
	ctx := waitCtx(t)

	held, err := Acquire(ctx, rs[0].client, wakeResource, Owner("u@x.com", "writer"), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	granted := make(chan time.Time, 2)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		r := rs[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			lk, aerr := AcquireShared(ctx, r.client, wakeResource, Owner("u@x.com", fmt.Sprintf("r%d", i)), 30*time.Second)
			if aerr != nil {
				t.Errorf("reader %d: %v", i, aerr)
				return
			}
			granted <- time.Now()
			<-release
			_ = r.client.Unlock(ctx, lk.ID)
		}()
		inLine(t, mr, i+1)
		time.Sleep(2 * time.Millisecond)
	}
	if err := rs[0].client.Unlock(ctx, held.ID); err != nil {
		t.Fatal(err)
	}
	first := <-granted
	select {
	case second := <-granted:
		if d := second.Sub(first); d >= lostWakeBackstop {
			t.Errorf("the second reader came %v after the first; the backstop, not the hand-off, let it in", d)
		}
	case <-time.After(3 * time.Second):
		t.Error("the second reader waited for the first to finish")
	}
	close(release)
	wg.Wait()
}

// freezingProxy stands between a client and the store. freeze makes every
// connection open at that moment stop carrying data both ways while staying
// open: the peer that vanished without a word. Connections made after it pass.
type freezingProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	conns  []*proxied
}

type proxied struct{ frozen atomic.Bool }

func newFreezingProxy(t *testing.T, target string) *freezingProxy {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &freezingProxy{ln: ln, target: target}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go p.serve(t, c)
		}
	}()
	return p
}

func (p *freezingProxy) serve(t *testing.T, c net.Conn) {
	defer func() { _ = c.Close() }()
	var d net.Dialer
	up, err := d.DialContext(t.Context(), "tcp", p.target)
	if err != nil {
		return
	}
	defer func() { _ = up.Close() }()
	pc := &proxied{}
	p.mu.Lock()
	p.conns = append(p.conns, pc)
	p.mu.Unlock()
	pipe := func(dst, src net.Conn) {
		buf := make([]byte, 32<<10)
		for {
			n, rerr := src.Read(buf)
			if n > 0 && !pc.frozen.Load() {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}
	go pipe(up, c)
	pipe(c, up)
}

func (p *freezingProxy) freeze() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		c.frozen.Store(true)
	}
}

func (p *freezingProxy) addr() string { return p.ln.Addr().String() }

// A subscription whose connection stops carrying data without closing is
// found by its unanswered ping and taken again on a fresh connection, and
// every waiter is woken, with no release published.
func TestASilentlyDeadSubscriptionIsTakenAgain(t *testing.T) {
	mr := miniredis.RunT(t)
	proxy := newFreezingProxy(t, mr.Addr())
	rdb := redis.NewClient(&redis.Options{Addr: proxy.addr()})
	b := NewRedisBackend(rdb)
	b.wakes.ping = 200 * time.Millisecond
	t.Cleanup(func() { _ = b.Close() })
	ctx := t.Context()

	wakes, stop, err := b.Wakes(ctx, wakeResource, "t1")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	proxy.freeze()
	frozeAt := time.Now()
	select {
	case <-wakes:
	case <-time.After(10 * time.Second):
		t.Fatal("the waiter was never woken; the dead subscription was kept")
	}
	// Silence, the ping, no answer: two periods, and the fresh subscription.
	if took := time.Since(frozeAt); took > 3*b.wakes.ping+time.Second {
		t.Errorf("found the dead subscription after %v, want within about two ping periods", took)
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
		t.Fatal("a release on the fresh subscription reached nobody")
	}
}
