package mdbox

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// lapsingLocker grants each resource to a single-goroutine test; lapse drops
// the map lease, after which its Renew fails.
type lapsingLocker struct {
	mutexLocker
	mu     sync.Mutex
	held   map[string]bool
	lapsed bool
}

func newLapsingLocker() *lapsingLocker {
	return &lapsingLocker{held: map[string]bool{}}
}

func (l *lapsingLocker) Lock(_ context.Context, r, _ string, _ time.Duration) (locks.Lock, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.held[r] = true
	return locks.Lock{ID: r, Resource: r}, nil
}
func (l *lapsingLocker) Unlock(_ context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.held, id)
	return nil
}
func (l *lapsingLocker) Renew(_ context.Context, id string, _ time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lapsed && id == locks.MdboxMapKey("u@x.io") {
		return locks.ErrExpired
	}
	return nil
}
func (l *lapsingLocker) HoldsResource(r string) (locks.HoldMode, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held[r] && !(l.lapsed && r == locks.MdboxMapKey("u@x.io")) {
		return locks.HoldExclusive, true
	}
	return locks.HoldNone, false
}

// lapseAndWait waits for the hold's context to end, not for the failed Renew:
// the context is cancelled after Renew returns, and a check between the two
// still sees the hold.
func (l *lapsingLocker) lapseAndWait(t *testing.T, ctx context.Context) {
	l.mu.Lock()
	l.lapsed = true
	l.mu.Unlock()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("lapsed lease was never noticed")
	}
}

func shortRebuildLease(t *testing.T) {
	prevTTL, prevRenew := rebuildLockTTL, rebuildLockRenew
	rebuildLockTTL, rebuildLockRenew = time.Second, time.Millisecond
	t.Cleanup(func() { rebuildLockTTL, rebuildLockRenew = prevTTL, prevRenew })
}

// A rebuild whose lease lapsed must not write the map: a save may already be in
// the storage it scanned (#2234).
func TestRebuildPastALapsedLeaseLeavesTheMapAlone(t *testing.T) {
	shortRebuildLease(t)
	l := newLapsingLocker()
	box, idx := newBoxAndIndex(t, filepath.Join(t.TempDir(), "home"), WithLocker(l))
	deliverMsg(t, box, idx, "INBOX", "referenced\r\n")
	// Unreferenced: a rebuild that writes the map sets it to zero-ref.
	if _, _, _, err := box.Save("INBOX", strings.NewReader("orphan\r\n"), 0, 0, nil, nil, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	beforeRebuildCommit = func(ctx context.Context) { l.lapseAndWait(t, ctx) }
	t.Cleanup(func() { beforeRebuildCommit = nil })

	_, err := box.RebuildStorage(mailboxbase.Open(box, idx), idx, false)
	if !errors.Is(err, locks.ErrExpired) {
		t.Errorf("rebuild past a lapsed lease: %v, want the lost lease reported", err)
	}
	m, _ := box.openMap()
	if z := m.CompactGarbage(); len(z) != 0 {
		t.Fatalf("map written past a lapsed lease: zero-ref %v", z)
	}
}

// A heal whose lease lapsed must not expunge: a purge may have moved what its
// scan would read as missing (#2234).
func TestHealPastALapsedLeaseExpungesNothing(t *testing.T) {
	shortRebuildLease(t)
	l := newLapsingLocker()
	box, idx := newBoxAndIndex(t, filepath.Join(t.TempDir(), "home"), WithLocker(l))
	deliverMsg(t, box, idx, "INBOX", "keep me\r\n")
	f, _ := idx.OpenFolder("INBOX", 0)
	uid, _ := idx.AllocateUID(f.ID)
	if err := idx.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, MapUID: 999999, Size: 4, GUID: [16]byte{7}}); err != nil {
		t.Fatal(err)
	}
	beforeHealScan = func(ctx context.Context) { l.lapseAndWait(t, ctx) }
	t.Cleanup(func() { beforeHealScan = nil })

	healBarrier.Range(func(k, _ any) bool { healBarrier.Delete(k); return true })
	if _, err := box.HealCorruptFolder(mailboxbase.Open(box, idx), idx, f); err == nil {
		t.Error("heal past a lapsed lease reported success")
	}
	if got := folderCount(t, idx, "INBOX"); got != 2 {
		t.Fatalf("INBOX = %d after a heal past a lapsed lease, want 2", got)
	}
}
