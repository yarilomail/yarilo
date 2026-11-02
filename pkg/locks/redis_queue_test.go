package locks

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func redisQueueBackend(t *testing.T) (*RedisBackend, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedisBackend(rdb), mr
}

// Contenders queue in arrival order across replicas; the one at the front is
// the only one whose turn it is, and an empty line lets anybody try.
func TestRedisQueueKeepsArrivalOrder(t *testing.T) {
	b, mr := redisQueueBackend(t)
	ctx := context.Background()
	mr.SetTime(time.Unix(1_700_000_000, 0))
	for i, ticket := range []string{"t1", "t2", "t3"} {
		mr.SetTime(time.Unix(1_700_000_000, int64(i)*int64(time.Millisecond)))
		rank, err := b.Enqueue(ctx, "box", ticket)
		if err != nil || rank != i {
			t.Fatalf("enqueue %s = %d, %v; want %d", ticket, rank, err, i)
		}
	}
	if rank, _ := b.Enqueue(ctx, "box", "t2"); rank != 1 {
		t.Errorf("enqueueing t2 again moved it to %d", rank)
	}
	for ticket, want := range map[string]bool{"t1": true, "t2": false, "t3": false} {
		if got, err := b.AtFront(ctx, "box", ticket); err != nil || got != want {
			t.Errorf("AtFront(%s) = %v, %v; want %v", ticket, got, err, want)
		}
	}
	if got, _ := b.AtFront(ctx, "other", "anyone"); !got {
		t.Error("an empty line kept a contender waiting")
	}
}

// A ticket abandoned for longer than the window ages out of the line, so it
// cannot hold everybody behind it.
func TestRedisQueueDropsAbandonedTickets(t *testing.T) {
	b, mr := redisQueueBackend(t)
	ctx := context.Background()
	mr.SetTime(time.Unix(1_700_000_000, 0))
	if _, err := b.Enqueue(ctx, "box", "gone"); err != nil {
		t.Fatal(err)
	}
	mr.SetTime(time.Unix(1_700_000_000, 0).Add(ticketTTL + time.Second))
	if rank, err := b.Enqueue(ctx, "box", "fresh"); err != nil || rank != 0 {
		t.Fatalf("enqueue after the window = %d, %v; want the front", rank, err)
	}
	if front, _ := b.AtFront(ctx, "box", "fresh"); !front {
		t.Error("the abandoned ticket still holds the front")
	}
}

// Leaving the line hands the turn to whoever stands first now, and only that
// contender is woken; leaving with a ticket the line does not hold wakes
// nobody.
func TestRedisQueueHandsTheTurnOn(t *testing.T) {
	b, mr := redisQueueBackend(t)
	ctx := context.Background()
	for i, ticket := range []string{"t1", "t2", "t3"} {
		mr.SetTime(time.Unix(1_700_000_000, int64(i)*int64(time.Millisecond)))
		if _, err := b.Enqueue(ctx, "box", ticket); err != nil {
			t.Fatal(err)
		}
	}
	wake2, stop2, err := b.Wakes(ctx, "box", "t2")
	if err != nil {
		t.Fatal(err)
	}
	defer stop2()
	wake3, stop3, err := b.Wakes(ctx, "box", "t3")
	if err != nil {
		t.Fatal(err)
	}
	defer stop3()

	if err := b.Dequeue(ctx, "box", "nobody"); err != nil {
		t.Fatal(err)
	}
	if err := b.Dequeue(ctx, "box", "t1"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wake2:
	case <-time.After(2 * time.Second):
		t.Fatal("t2 was not woken when t1 left")
	}
	select {
	case <-wake3:
		t.Fatal("t3 was woken for a turn that is t2's")
	case <-time.After(100 * time.Millisecond):
	}
	if front, _ := b.AtFront(ctx, "box", "t2"); !front {
		t.Error("t2 is not at the front after t1 left")
	}
	stop2()
	stop2() // a second stop is harmless
	if _, ok := <-wake2; ok {
		t.Error("the wake channel stayed open after stop")
	}
}
