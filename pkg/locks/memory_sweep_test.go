package locks

import (
	"context"
	"testing"
	"time"
)

// sweepBound is how late the ticker it replaced could remove an expired lock.
const sweepBound = 100 * time.Millisecond

func (b *MemoryBackend) held(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.locks[id]
	return ok
}

// goneBy reports how long after start the lock left the map, polling until limit.
func goneBy(b *MemoryBackend, id string, start time.Time, limit time.Duration) (time.Duration, bool) {
	for time.Since(start) < limit {
		if !b.held(id) {
			return time.Since(start), true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return 0, false
}

// An idle backend has no timer: a second with nothing held wakes the sweeper
// not once (#2515).
func TestMemorySweeperSleepsWhenIdle(t *testing.T) {
	b := NewMemoryBackend()
	defer b.Close()
	time.Sleep(time.Second)
	if n := b.sweepRuns.Load(); n != 0 {
		t.Errorf("the sweeper woke %d times in a second with no lock held, want 0", n)
	}
}

// The sweeper removes an expired lock by itself, no later than the ticker did:
// alone, after a lock added while it slept on a later one, and after a renew.
func TestMemorySweeperRemovesOnTime(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, b *MemoryBackend) (id string, start time.Time, ttl time.Duration)
	}{
		{"alone", func(t *testing.T, b *MemoryBackend) (string, time.Time, time.Duration) {
			start := time.Now()
			id, _, err := b.Acquire(ctx, "r", "o", "write", "", 200*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			return id, start, 200 * time.Millisecond
		}},
		{"added while the sweeper sleeps on a later one", func(t *testing.T, b *MemoryBackend) (string, time.Time, time.Duration) {
			if _, _, err := b.Acquire(ctx, "long", "o", "write", "", time.Minute); err != nil {
				t.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond)
			start := time.Now()
			id, _, err := b.AcquireShared(ctx, "short", "o", "read", "", 200*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			return id, start, 200 * time.Millisecond
		}},
		{"renewed", func(t *testing.T, b *MemoryBackend) (string, time.Time, time.Duration) {
			id, _, err := b.Acquire(ctx, "r", "o", "write", "", 150*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond)
			start := time.Now()
			if err := b.Renew(ctx, id, 400*time.Millisecond); err != nil {
				t.Fatal(err)
			}
			return id, start, 400 * time.Millisecond
		}},
		{"renewed shorter", func(t *testing.T, b *MemoryBackend) (string, time.Time, time.Duration) {
			id, _, err := b.Acquire(ctx, "r", "o", "write", "", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond)
			start := time.Now()
			if err := b.Renew(ctx, id, 200*time.Millisecond); err != nil {
				t.Fatal(err)
			}
			return id, start, 200 * time.Millisecond
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := NewMemoryBackend()
			defer b.Close()
			id, start, ttl := tc.run(t, b)
			after, gone := goneBy(b, id, start, ttl+sweepBound+time.Second)
			switch {
			case !gone:
				t.Fatalf("the lock was still held %v after a TTL of %v", ttl+sweepBound+time.Second, ttl)
			case after < ttl:
				t.Errorf("removed %v after its TTL started, before the TTL of %v", after, ttl)
			case after > ttl+sweepBound:
				t.Errorf("removed %v after its TTL started, later than %v + %v", after, ttl, sweepBound)
			}
		})
	}
}
