package locks_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
)

// Waiting acquires reuse their connections: a call used to open one and close
// it again, and every open resolved the service name -- 376 s of a 928 s
// waiting profile (#1875).
func TestWaitingAcquiresKeepTheirConnection(t *testing.T) {
	l, cleanup := memoryFactory(t)
	defer cleanup()
	ctx := locks.WithSite(context.Background(), "wait-pool-row")

	before := locks.Dials("wait")
	for i := 0; i < 20; i++ {
		lk, err := l.(*locks.Client).LockWaiting(ctx, "res", locks.Owner("u1@d.test", "wait-row"), time.Minute, time.Second, false)
		if err != nil {
			t.Fatalf("waiting acquire %d: %v", i, err)
		}
		if err := l.Unlock(ctx, lk.ID); err != nil {
			t.Fatalf("unlock %d: %v", i, err)
		}
	}
	dials := locks.Dials("wait") - before
	if dials > 1 {
		t.Errorf("twenty waiting acquires opened %v connections, want one kept", dials)
	}
	if dials == 0 {
		t.Error("no dial was counted at all; the counter is not on the path")
	}
}

// A wait that ends without its answer -- the caller gave up -- must not put the
// connection back for the next caller: the server may still write the grant
// into it.
func TestAnAbandonedWaitDoesNotPoisonThePool(t *testing.T) {
	l, cleanup := memoryFactory(t)
	defer cleanup()
	client := l.(*locks.Client)
	ctx := locks.WithSite(context.Background(), "wait-pool-row")

	held, err := client.LockWaiting(ctx, "res", locks.Owner("u1@d.test", "holder"), time.Minute, time.Second, false)
	if err != nil {
		t.Fatal(err)
	}

	// Somebody else waits for the same resource and gives up.
	short, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	_, werr := client.LockWaiting(short, "res", locks.Owner("u1@d.test", "waiter"), time.Minute, 5*time.Second, false)
	cancel()
	if werr == nil {
		t.Fatal("the waiter was granted a lock somebody else holds")
	}

	if err := client.Unlock(ctx, held.ID); err != nil {
		t.Fatal(err)
	}
	// The next caller must get a clean answer, not the grant meant for the one
	// that walked away.
	next, err := client.LockWaiting(ctx, "res", locks.Owner("u1@d.test", "next"), time.Minute, 2*time.Second, false)
	if err != nil {
		t.Fatalf("the acquire after an abandoned wait failed: %v", err)
	}
	if next.Owner != locks.Owner("u1@d.test", "next") {
		t.Errorf("the lock came back owned by %q, want the new caller", next.Owner)
	}
	_ = client.Unlock(ctx, next.ID)
}

// The pool is per process with a ceiling: fifty callers waiting at once hold
// at most the configured number of connections, not one each.
func TestTheWaitPoolHoldsItsCeiling(t *testing.T) {
	l, cleanup := memoryFactoryOpts(t, locks.WithWaitPoolSize(4))
	defer cleanup()
	client := l.(*locks.Client)
	ctx := locks.WithSite(context.Background(), "wait-pool-row")

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lk, err := client.LockWaiting(ctx, "res-"+strconv.Itoa(i%7), locks.Owner("u1@d.test", strconv.Itoa(i)),
				time.Minute, 3*time.Second, false)
			if err != nil {
				return
			}
			_ = client.Unlock(ctx, lk.ID)
		}(i)
	}
	wg.Wait()

	if n := client.WaitConnections(); n > 4 {
		t.Errorf("fifty callers left %d connections in a pool of 4", n)
	}
}

// A connection nobody has used for the idle timeout is closed rather than kept
// open for a burst that happened once.
func TestAnIdleWaitConnectionIsClosed(t *testing.T) {
	l, cleanup := memoryFactory(t)
	defer cleanup()
	client := l.(*locks.Client)
	ctx := locks.WithSite(context.Background(), "wait-pool-row")

	lk, err := client.LockWaiting(ctx, "res", locks.Owner("u1@d.test", "one"), time.Minute, time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Unlock(ctx, lk.ID)
	if client.WaitConnections() != 1 {
		t.Fatalf("after one waiting acquire the pool holds %d connections, want 1", client.WaitConnections())
	}

	locks.AgeWaitPool(client, 10*time.Minute)
	before := locks.Dials("wait")
	lk2, err := client.LockWaiting(ctx, "res", locks.Owner("u1@d.test", "two"), time.Minute, time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Unlock(ctx, lk2.ID)
	if locks.Dials("wait")-before != 1 {
		t.Error("the idle connection was reused instead of being closed and replaced")
	}
}

// A waiting call and the waiting call after it share one connection: the
// server answers, lets the connection go before writing, and serves the next
// command on it. Back-to-back waits on purpose -- an UNLOCK would travel on
// the control pool and prove nothing about this connection.
func TestAWaitRightAfterAWaitIsServed(t *testing.T) {
	l, cleanup := memoryFactoryOpts(t, locks.WithWaitPoolSize(1))
	defer cleanup()
	client := l.(*locks.Client)
	ctx := locks.WithSite(context.Background(), "wait-pool-row")

	// The window between answering and letting the connection go is widened,
	// so the order of those two is a property this row measures rather than
	// one argued from the code: with the answer written first, the watcher is
	// still in Read when the next command arrives and eats it.
	locks.SetWatchStopDelay(200 * time.Millisecond)
	defer locks.SetWatchStopDelay(0)

	// Repeated: the failure this guards is a race between the server's watcher
	// and the next command, and one pass proves little.
	for i := 0; i < 5; i++ {
		lk, err := client.LockWaiting(ctx, "res", locks.Owner("u1@d.test", "w"), time.Minute, 2*time.Second, false)
		if err != nil {
			t.Fatalf("wait %d: %v", i, err)
		}
		if err := client.Unlock(ctx, lk.ID); err != nil {
			t.Fatalf("unlock %d: %v", i, err)
		}
	}
	if n := locks.Dials("wait"); n == 0 {
		t.Fatal("no waiting connection was ever dialled; the row proves nothing")
	}
}

// The lock service restarts: every kept connection dies at once, and the first
// command after it goes through -- the client replaces what it holds instead
// of failing the caller twice.
func TestTheFirstCommandAfterARestartGoesThrough(t *testing.T) {
	l, cleanup, restart := memoryFactoryRestartable(t)
	defer cleanup()
	client := l.(*locks.Client)
	ctx := locks.WithSite(context.Background(), "wait-pool-row")

	lk, err := client.LockWaiting(ctx, "res", locks.Owner("u1@d.test", "before"), time.Minute, time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = client.Unlock(ctx, lk.ID)

	restart()

	var lerr error
	for i := 0; i < 3; i++ { // the client retries once; the row allows for the dial
		var lk2 locks.Lock
		lk2, lerr = client.LockWaiting(ctx, "res", locks.Owner("u1@d.test", "after"), time.Minute, 2*time.Second, false)
		if lerr == nil {
			_ = client.Unlock(ctx, lk2.ID)
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if lerr != nil {
		t.Errorf("the first commands after a restart all failed: %v", lerr)
	}
}
