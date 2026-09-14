package lmtp

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// countingLocker counts the acquisitions the lock service would see.
type countingLocker struct {
	locks.Locker
	taken int
	held  map[string]locks.HoldMode
}

func (l *countingLocker) Lock(_ context.Context, resource, owner string, _ time.Duration) (locks.Lock, error) {
	l.taken++
	l.held[resource] = locks.HoldExclusive
	return locks.Lock{ID: resource, Resource: resource, Owner: owner}, nil
}

func (l *countingLocker) LockShared(ctx context.Context, resource, owner string, ttl time.Duration) (locks.Lock, error) {
	return l.Lock(ctx, resource, owner, ttl)
}

func (l *countingLocker) Unlock(_ context.Context, id string) error {
	delete(l.held, id)
	return nil
}

func (l *countingLocker) HoldsResource(resource string) (locks.HoldMode, bool) {
	m, ok := l.held[resource]
	return m, ok
}

func (l *countingLocker) Emit(context.Context, string, locks.EventType, string) error { return nil }

// A delivery takes the folder once: it took it three times — uid, modseq,
// record — with two windows between them (#1706).
func TestADeliveryTakesTheFolderOnce(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	lk := &countingLocker{held: map[string]locks.HoldMode{}}
	store := maildir.New(maildir.WithLocker(lk)).OpenUser(info)
	idx := file.New(file.WithLocker(lk)).OpenUser(info)
	t.Cleanup(func() { _ = store.Close(); _ = idx.Close() })
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	// The door a delivery uses: it adds a message and settles nothing, so the
	// walk buys nothing and the hold it costs is the one this row counts.
	box := mailboxbase.Open(store, idx, mailboxbase.SaveOnly())
	if _, err := box.Folder("INBOX", 1); err != nil {
		t.Fatal(err)
	}

	const raw = "From: a@b\r\nSubject: one hold\r\n\r\nbody\r\n"
	lk.taken = 0
	scansBefore := testutil.ToFloat64(mailboxbase.MetricReconcile.WithLabelValues("scanned"))
	uid, _, _, err := deliverOne(box, "INBOX", bytes.NewReader([]byte(raw)), int64(len(raw)), nil, info.Username, "x@y", nil)
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if uid == 0 {
		t.Fatal("the delivery reported uid 0")
	}
	if lk.taken != 1 {
		t.Errorf("the delivery took the folder %d times, want 1", lk.taken)
	}
	if n := testutil.ToFloat64(mailboxbase.MetricReconcile.WithLabelValues("scanned")) - scansBefore; n != 0 {
		t.Errorf("the delivery walked the folder %v times, want none", n)
	}

	// And the message is there, named, as any delivery must leave it.
	msgs, err := box.Messages(1, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("the folder holds %d records", len(msgs))
	}
	name, perr := box.MessagePath("INBOX", msgs[0])
	if perr != nil || name == "" {
		t.Fatalf("the delivered record names no file: %q %v", name, perr)
	}
	rc, oerr := box.OpenMessage("INBOX", msgs[0])
	if oerr != nil {
		t.Fatalf("the delivered message cannot be read: %v", oerr)
	}
	defer rc.Close() //nolint:errcheck
	body := make([]byte, len(raw))
	if _, rerr := rc.Read(body); rerr != nil && !strings.Contains(rerr.Error(), "EOF") {
		t.Fatalf("read: %v", rerr)
	}
}
