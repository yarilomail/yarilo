package dboxv2

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	fileidx "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// markLocker counts what the lock service would see.
type markLocker struct {
	locks.Locker
	taken int
	held  map[string]locks.HoldMode
}

func (l *markLocker) Lock(_ context.Context, resource, owner string, _ time.Duration) (locks.Lock, error) {
	l.taken++
	l.held[resource] = locks.HoldExclusive
	return locks.Lock{ID: resource, Resource: resource, Owner: owner}, nil
}

func (l *markLocker) LockShared(ctx context.Context, resource, owner string, ttl time.Duration) (locks.Lock, error) {
	return l.Lock(ctx, resource, owner, ttl)
}

func (l *markLocker) Unlock(_ context.Context, id string) error { delete(l.held, id); return nil }

func (l *markLocker) HoldsResource(resource string) (locks.HoldMode, bool) {
	m, ok := l.held[resource]
	return m, ok
}

// A mark is not a session: a store that just reported corruption is not swept
// on the way to the marker (#1715). sdbox: maildir never reaches the open.
func TestMarkingCorruptionDoesNotSettleTheFolder(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "alice@example.com", Home: home}
	lk := &markLocker{held: map[string]locks.HoldMode{}}
	store := New(WithLocker(lk)).OpenUser(info)
	idx := fileidx.New(fileidx.WithLocker(lk)).OpenUser(info)
	t.Cleanup(func() { _ = store.Close(); _ = idx.Close() })
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	box := mailboxbase.Open(store, idx)
	if _, err := box.Folder("INBOX", 1); err != nil {
		t.Fatal(err)
	}

	// The priming open swept and stamped; remove the stamp, so a sweep on the
	// way to the marker would leave a fresh one.
	stamp := filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails", mailbox.SweepStampName)
	if err := os.Remove(stamp); err != nil {
		t.Fatalf("the priming open did not sweep, so this row measures nothing: %v", err)
	}

	if !box.MarkCorruptOnFetchErr("INBOX", mailbox.ErrCorruptStorage) {
		t.Fatal("the folder was not marked, so this row measures nothing")
	}
	if _, err := os.Stat(stamp); err == nil {
		t.Error("the mark swept the folder; it opened it as a session would")
	}
}
