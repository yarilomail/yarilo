package locks

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// countingLockerInner counts what reaches the service.
type countingLockerInner struct {
	Locker
	mu    sync.Mutex
	locks map[string]int
}

func (c *countingLockerInner) Lock(ctx context.Context, resource, owner string, ttl time.Duration) (Lock, error) {
	c.mu.Lock()
	c.locks[resource]++
	c.mu.Unlock()
	return c.Locker.Lock(ctx, resource, owner, ttl)
}

func (c *countingLockerInner) count(resource string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.locks[resource]
}

// One lease per account, whatever the account's folders are asked to do: the
// service sees one acquisition instead of one per command (#1840).
func TestAnAccountReachesTheServiceOnceUnderALease(t *testing.T) {
	backend := NewMemoryBackend()
	t.Cleanup(func() { _ = backend.Close() })
	client := standOver(t, backend, nil)
	inner := &countingLockerInner{Locker: client, locks: map[string]int{}}
	l := NewLeased(inner, Owner("pod-a", "lease"))

	ctx := WithSite(context.Background(), "expunge")
	const account = "u@x.com"
	for i := 0; i < 40; i++ {
		lk, err := l.Lock(ctx, MailboxKey(account, "INBOX"), Owner(account, fmt.Sprintf("s%d", i)), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if uerr := l.Unlock(ctx, lk.ID); uerr != nil {
			t.Fatal(uerr)
		}
	}
	if got := inner.count(MailboxKey(account, "INBOX")); got != 0 {
		t.Errorf("the folder reached the service %d times; it is serialised in process", got)
	}
	if got := inner.count(UserKey(account)); got != 1 {
		t.Errorf("the account was leased %d times for 40 commands, want 1", got)
	}
}

// Two holders of one folder do not overlap, lease or no lease.
func TestALeasedFolderStillSerialises(t *testing.T) {
	backend := NewMemoryBackend()
	t.Cleanup(func() { _ = backend.Close() })
	client := standOver(t, backend, nil)
	l := NewLeased(client, Owner("pod-a", "lease"))

	ctx := WithSite(context.Background(), "expunge")
	key := MailboxKey("u@x.com", "INBOX")
	var inside, maxInside int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lk, err := l.Lock(ctx, key, Owner("u@x.com", fmt.Sprintf("s%d", i)), time.Minute)
			if err != nil {
				t.Errorf("lock: %v", err)
				return
			}
			mu.Lock()
			inside++
			if inside > maxInside {
				maxInside = inside
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			inside--
			mu.Unlock()
			if uerr := l.Unlock(ctx, lk.ID); uerr != nil {
				t.Errorf("unlock: %v", uerr)
			}
		}(i)
	}
	wg.Wait()
	if maxInside != 1 {
		t.Errorf("%d holders were inside the folder at once, want 1", maxInside)
	}
}

// Two callers arriving at once take one lease between them, and neither waits
// on the other's lock: the first takes it, the rest wait on its result (#1840).
func TestConcurrentCallersTakeOneLeaseAndDoNotBlock(t *testing.T) {
	backend := NewMemoryBackend()
	t.Cleanup(func() { _ = backend.Close() })
	client := standOver(t, backend, nil)
	inner := &countingLockerInner{Locker: client, locks: map[string]int{}}
	l := NewLeased(inner, Owner("pod-a", "lease"))

	ctx := WithSite(context.Background(), "expunge")
	const account = "u@x.com"
	started := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lk, err := l.Lock(ctx, MailboxKey(account, "INBOX"), Owner(account, fmt.Sprintf("s%d", i)), time.Minute)
			if err != nil {
				t.Errorf("lock: %v", err)
				return
			}
			_ = l.Unlock(ctx, lk.ID)
		}(i)
	}
	wg.Wait()
	if took := time.Since(started); took > 5*time.Second {
		t.Errorf("twenty callers took %v; they waited on each other's lease", took)
	}
	if got := inner.count(UserKey(account)); got != 1 {
		t.Errorf("the account was leased %d times, want 1", got)
	}
}
