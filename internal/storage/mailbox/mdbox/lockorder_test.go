package mdbox

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// countingLocker counts acquisitions per resource, so "once per expunge" is a
// number rather than a claim.
type countingLocker struct {
	mu      sync.Mutex
	count   map[string]int
	holding map[string]bool
}

func newCountingLocker() *countingLocker {
	return &countingLocker{count: map[string]int{}, holding: map[string]bool{}}
}

func (l *countingLocker) Lock(_ context.Context, resource, _ string, _ time.Duration) (locks.Lock, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.count[resource]++
	return locks.Lock{ID: resource, Resource: resource}, nil
}

func (l *countingLocker) LockShared(ctx context.Context, r, o string, ttl time.Duration) (locks.Lock, error) {
	return l.Lock(ctx, r, o, ttl)
}
func (l *countingLocker) Unlock(context.Context, string) error               { return nil }
func (l *countingLocker) Renew(context.Context, string, time.Duration) error { return nil }
func (l *countingLocker) Close() error                                       { return nil }
func (l *countingLocker) Subscribe(context.Context, string) (<-chan locks.Event, error) {
	return nil, nil
}
func (l *countingLocker) Emit(context.Context, string, locks.EventType, string) error { return nil }
func (l *countingLocker) IncrementCounter(context.Context, string, int64) (int64, error) {
	return 0, nil
}

func (l *countingLocker) HoldsResource(resource string) (locks.HoldMode, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holding[resource] {
		return locks.HoldExclusive, true
	}
	return locks.HoldNone, false
}

func (l *countingLocker) taken(resource string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.count[resource]
}

// An expunge of many messages costs one map lock, not one per body: each is a
// round trip to the lock service under the folder hold (#1884).
//
// Driven through ExpungeMarked, not through the batch call: the defect was the
// caller looping, so a row that calls the batch itself proves nothing.
func TestAnExpungeTakesTheMapLockOnce(t *testing.T) {
	rec := newCountingLocker()
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "alice@example.com", Home: home, SessionID: "s1"}
	store := New(WithLocker(rec)).OpenUser(info)
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	idx := file.New().OpenUser(info)
	t.Cleanup(func() { _ = idx.Close() })
	box := mailboxbase.Open(store, idx)

	f, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	msgs := make([]*mailbox.MessageMeta, 0, 8)
	for i := 0; i < 8; i++ {
		body := "From: a@b\r\nSubject: m\r\n\r\nbody\r\n"
		uid, uerr := idx.AllocateUID(f.ID)
		if uerr != nil {
			t.Fatal(uerr)
		}
		saved, vsize, guid, serr := store.Save("INBOX", strings.NewReader(body), uid, int64(len(body)), nil, nil, [16]byte{})
		if serr != nil {
			t.Fatal(serr)
		}
		m := &mailbox.MessageMeta{UID: uid, Size: uint32(len(body)), VSize: vsize, GUID: guid}
		if rerr := box.RecordDelivered(f, "INBOX", saved, m); rerr != nil {
			t.Fatal(rerr)
		}
		msgs = append(msgs, m)
	}

	mapKey := locks.MdboxMapKey(info.Username)
	before := rec.taken(mapKey)
	removed, failed, eerr := box.ExpungeMarked(f, "INBOX", msgs)
	if eerr != nil || failed != 0 || len(removed) != len(msgs) {
		t.Fatalf("expunge removed %d and failed %d (%v), want %d removed", len(removed), failed, eerr, len(msgs))
	}

	if got := rec.taken(mapKey) - before; got != 1 {
		t.Errorf("expunging %d messages took the map lock %d times, want 1", len(msgs), got)
	}
}

// The guard refuses the inverted order by name, at the moment it is asked for,
// rather than leaving two 30s waits to meet each other.
func TestAFolderUnderTheMapIsRefusedByName(t *testing.T) {
	rec := newCountingLocker()
	b := New(WithLocker(rec))
	u := b.OpenUser(&mailbox.UserInfo{
		Username: "alice@example.com", Home: t.TempDir(), SessionID: "s1",
	}).(*userMailbox)
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}

	// This goroutine holds the map, as the inverted path did.
	rec.mu.Lock()
	rec.holding[locks.MdboxMapKey(u.username)] = true
	rec.mu.Unlock()

	err := u.withMailboxLock("INBOX", func() error { return nil })
	if err == nil {
		t.Fatal("a folder lock was taken under the map lock")
	}
	if !errors.Is(err, ErrLockOrder) {
		t.Fatalf("error %v is not the lock-order refusal", err)
	}
	for _, want := range []string{locks.MdboxMapKey(u.username), locks.MailboxKey(u.username, "INBOX")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %v does not name %s", err, want)
		}
	}
}

// Two sessions of one user, opposite orders, driven by channels rather than
// sleeps: the pair that deadlocked is refused at once instead of waiting out
// two 30-second timeouts.
func TestTheInvertedOrderIsRefusedRatherThanDeadlocked(t *testing.T) {
	rec := newCountingLocker()
	b := New(WithLocker(rec))
	u := b.OpenUser(&mailbox.UserInfo{
		Username: "alice@example.com", Home: t.TempDir(), SessionID: "s1",
	}).(*userMailbox)
	if err := u.Init(); err != nil {
		t.Fatal(err)
	}

	folderTaken := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)

	// One: folder, then map -- the order the tree takes.
	go func() {
		done <- u.withMailboxLock("INBOX", func() error {
			close(folderTaken)
			<-release
			return nil
		})
	}()
	<-folderTaken

	// Two: holding the map, asking for the same folder -- the inversion.
	rec.mu.Lock()
	rec.holding[locks.MdboxMapKey(u.username)] = true
	rec.mu.Unlock()
	invErr := u.withMailboxLock("INBOX", func() error { return nil })

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the ordered path failed: %v", err)
	}
	if !errors.Is(invErr, ErrLockOrder) {
		t.Fatalf("the inverted path answered %v, want the lock-order refusal", invErr)
	}
}
