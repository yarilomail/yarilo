package locks_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/yarilomail/yarilo/v2/pkg/locks"
)

// holdUntilEnded runs fn under WithLock and reports when its context ended.
func holdUntilEnded(l locks.Locker, resource string) (inside chan struct{}, ended chan time.Time, errc chan error) {
	inside, ended, errc = make(chan struct{}), make(chan time.Time, 1), make(chan error, 1)
	go func() {
		errc <- locks.WithLock(locks.WithSite(context.Background(), "test"), l, resource, locks.Owner("holder@example.com", locks.NewID()), 15*time.Second, 5*time.Second,
			func(ctx context.Context) error {
				close(inside)
				<-ctx.Done()
				ended <- time.Now()
				return ctx.Err()
			})
	}()
	return inside, ended, errc
}

// A service that comes back with fresh state grants the resource again at
// once. The hold granted before ends as soon as the client reconnects, not at
// its next renew five seconds on, and the client counts it (#2425).
func TestAHoldEndsWhenTheServiceComesBackWithFreshState(t *testing.T) {
	l, cleanup, restart := memoryFactoryRestartable(t)
	defer cleanup()
	res := locks.ThreadsKey("u@example.com")
	before := locks.HoldsLost(res, "generation")

	inside, ended, errc := holdUntilEnded(l, res)
	<-inside
	restart()
	// Any call reconnects, and its handshake names the new generation.
	reconnected := time.Now()
	if lk, err := l.Lock(locks.WithSite(context.Background(), "test"), locks.ThreadsKey("other@example.com"), locks.Owner("other@example.com", locks.NewID()), time.Second); err == nil {
		_ = l.Unlock(context.Background(), lk.ID)
	}
	select {
	case at := <-ended:
		if took := at.Sub(reconnected); took > time.Second {
			t.Errorf("the hold ended %v after the reconnect; want at once, not at the renew tick", took)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the hold outlived the reconnect to a service with fresh state")
	}
	if err := <-errc; !errors.Is(err, locks.ErrLost) {
		t.Errorf("WithLock returned %v, want ErrLost", err)
	}
	if got := locks.HoldsLost(res, "generation") - before; got != 1 {
		t.Errorf("holds lost to a new generation grew by %v, want 1", got)
	}
}

// A hold granted after the restart belongs to the new state and stays.
func TestAHoldFromTheNewGenerationStays(t *testing.T) {
	l, cleanup, restart := memoryFactoryRestartable(t)
	defer cleanup()
	restart()
	res := locks.ThreadsKey("u@example.com")
	inside, ended, _ := holdUntilEnded(l, res)
	<-inside
	if lk, err := l.Lock(locks.WithSite(context.Background(), "test"), locks.ThreadsKey("other@example.com"), locks.Owner("other@example.com", locks.NewID()), time.Second); err == nil {
		_ = l.Unlock(context.Background(), lk.ID)
	}
	select {
	case <-ended:
		t.Fatal("a hold granted by the current generation was ended")
	case <-time.After(300 * time.Millisecond):
	}
}

// Redis keeps the lock state across a service restart, so its generation does
// too; it changes only when the keys are gone.
func TestTheRedisGenerationFollowsTheStore(t *testing.T) {
	mr := miniredis.RunT(t)
	gen := func() string {
		rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
		defer rdb.Close()
		g, err := locks.NewRedisBackend(rdb).Generation(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	first := gen()
	if first == "" || gen() != first {
		t.Fatalf("a second server over the same store named another generation than %q", first)
	}
	mr.FlushAll()
	if gen() == first {
		t.Error("the generation outlived the state it names")
	}
}
