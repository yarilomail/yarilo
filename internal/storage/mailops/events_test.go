package mailops

import (
	"context"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/locks/lockstest"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A change one user makes in a public folder is published on the store's key,
// where another user's session on that folder listens (#2600).
func TestASharedFolderEventReachesAnotherUser(t *testing.T) {
	dial := lockstest.Server(t)
	pub := t.TempDir()
	open := func(user string) (*mailbox.UserInfo, mailbox.Box) {
		base := &mailbox.UserInfo{Username: user, Home: t.TempDir(), SessionID: "s-" + user}
		ui, err := mailbox.NamespaceUserInfo(base, mailbox.Location{Driver: "maildir", Path: pub}, "/", mailbox.SharedByAll)
		if err != nil {
			t.Fatal(err)
		}
		return base, mailboxbase.Open(maildir.New().OpenUser(ui), file.New().OpenUser(ui))
	}
	alice, aliceBox := open("alice@example.com")
	_, bobBox := open("bob@example.com")

	ctx, cancel := context.WithTimeout(locks.WithSite(context.Background(), "test"), 5*time.Second)
	defer cancel()
	ch, err := dial().Subscribe(ctx, locks.MailboxKey(mailbox.LockUserOf(bobBox, "bob@example.com"), "News"))
	if err != nil {
		t.Fatal(err)
	}
	ops := New(Config{Locker: dial(), Protocol: "test"}, aliceBox, alice)
	ops.EmitMessageIn(aliceBox, "News", locks.EventDelivered, 7)
	select {
	case evt := <-ch:
		if evt.Type != locks.EventDelivered {
			t.Fatalf("event %v", evt.Type)
		}
	case <-ctx.Done():
		t.Fatal("bob's session on the public folder heard nothing of alice's delivery")
	}
}

// A folder-list change goes on a SharedByAll store's key and on nobody
// else's; in an owner's mailbox (user/<owner>/) it stays on the user's key,
// under the user's full name, where the user's own sessions hear it. A message
// event in an owner's mailbox goes on the owner's key, where the owner's
// sessions on that folder listen.
func TestEventKeysByKindOfStore(t *testing.T) {
	dial := lockstest.Server(t)
	pub := t.TempDir()
	alice := &mailbox.UserInfo{Username: "alice@example.com", Home: t.TempDir(), SessionID: "a"}
	bob := &mailbox.UserInfo{Username: "bob@example.com", Home: t.TempDir(), SessionID: "b"}
	sharedUI, err := mailbox.NamespaceUserInfo(bob, mailbox.Location{Driver: "maildir", Path: pub}, "/", mailbox.SharedByAll)
	if err != nil {
		t.Fatal(err)
	}
	ownerUI, err := mailbox.StampOwnerLocation(alice, bob, "maildir:%h/Maildir", '/')
	if err != nil {
		t.Fatal(err)
	}
	sharedBox := mailboxbase.Open(maildir.New().OpenUser(sharedUI), file.New().OpenUser(sharedUI))
	ownerBox := mailboxbase.Open(maildir.New().OpenUser(ownerUI), file.New().OpenUser(ownerUI))

	listen := func(key string) <-chan locks.Event {
		ctx, cancel := context.WithTimeout(locks.WithSite(context.Background(), "test"), 2*time.Second)
		t.Cleanup(cancel)
		ch, err := dial().Subscribe(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		return ch
	}
	heard := func(ch <-chan locks.Event) bool {
		select {
		case _, ok := <-ch:
			return ok
		case <-time.After(300 * time.Millisecond):
			return false
		}
	}
	ops := New(Config{Locker: dial(), Protocol: "test"}, nil, bob)

	t.Run("list, shared store", func(t *testing.T) {
		store, user := listen(locks.MailboxListKey("store:"+pub)), listen(locks.MailboxListKey("bob@example.com"))
		ops.EmitListIn(sharedBox, locks.EventMailboxCreate, "Public/New")
		if !heard(store) || heard(user) {
			t.Fatal("a shared store's list change must go on the store's key only")
		}
	})
	t.Run("list, owner's mailbox", func(t *testing.T) {
		owner, user := listen(locks.MailboxListKey("alice@example.com")), listen(locks.MailboxListKey("bob@example.com"))
		ops.EmitListIn(ownerBox, locks.EventMailboxCreate, "user/alice@example.com/New")
		if heard(owner) || !heard(user) {
			t.Fatal("a change bob makes in alice's mailbox is bob's view: his key, not hers")
		}
	})
	t.Run("message, owner's mailbox", func(t *testing.T) {
		owner := listen(locks.MailboxKey("alice@example.com", "Work"))
		ops.EmitMessageIn(ownerBox, "Work", locks.EventDelivered, 1)
		if !heard(owner) {
			t.Fatal("a message bob puts in alice's Work is not heard on alice's key")
		}
	})
}
