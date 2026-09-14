package mailboxbase_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	fileidx "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// LockShared counts like Lock: a read that reaches the service costs the same
// trip as a write, and this row counts trips.
func (l *countingLocker) LockShared(ctx context.Context, resource, owner string, ttl time.Duration) (locks.Lock, error) {
	return l.Lock(ctx, resource, owner, ttl)
}

// A settled folder was taking the lock on every open to be told there was
// nothing to do (#1778).
func TestASettledFolderIsOpenedWithoutTakingIt(t *testing.T) {
	root := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x.com", Home: filepath.Join(root, "x.com", "u")}
	lk := &countingLocker{held: map[string]locks.HoldMode{}}
	store := maildir.New(maildir.WithLocker(lk)).OpenUser(info)
	idx := fileidx.New(fileidx.WithLocker(lk)).OpenUser(info)
	t.Cleanup(func() { _ = store.Close(); _ = idx.Close() })
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	if err := store.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.OpenFolder("INBOX", 1); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mailboxbase.SetTestSyncTokens(8))
	box := mailboxbase.Open(store, idx)

	// First open: the pass runs and marks the folder.
	if _, err := box.Folder("INBOX", 1); err != nil {
		t.Fatal(err)
	}
	settle(t, filepath.Join(info.Home, "Maildir"), time.Now().Add(-time.Hour))
	if _, err := box.Folder("INBOX", 1); err != nil {
		t.Fatal(err)
	}

	lk.locks = 0
	if _, err := box.Folder("INBOX", 1); err != nil {
		t.Fatal(err)
	}
	if lk.locks != 0 {
		t.Errorf("opening a settled folder took it %d times, want none", lk.locks)
	}
}
