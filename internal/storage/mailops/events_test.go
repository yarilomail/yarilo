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
