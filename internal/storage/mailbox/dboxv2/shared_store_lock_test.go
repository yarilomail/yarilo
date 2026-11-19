package dboxv2

import (
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/locks/lockstest"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// publicHandle opens the public store as a session of user does: the
// namespace's UserInfo built on the user's, marked a shared store.
func publicHandle(t *testing.T, root, user string, dial func() locks.Locker) *userMailbox {
	t.Helper()
	base := &mailbox.UserInfo{Username: user, Home: t.TempDir()}
	loc, ok, err := mailbox.ParseLocation("sdbox:"+root, base)
	if err != nil || !ok {
		t.Fatalf("location: %v", err)
	}
	ui, err := mailbox.NamespaceUserInfo(base, loc, "/")
	if err != nil {
		t.Fatal(err)
	}
	mb := New(WithLocker(dial())).OpenUser(mailbox.SharedStore(ui))
	if err := mb.Init(); err != nil {
		t.Fatal(err)
	}
	return mb.(*userMailbox)
}

// Two users on one public sdbox folder exclude each other: while one holds
// the folder, the other's hold waits and gives up (#2598).
func TestTwoUsersOnOnePublicFolderExcludeEachOther(t *testing.T) {
	dial := lockstest.Server(t)
	root := t.TempDir()
	alice := publicHandle(t, root, "alice@example.com", dial)
	bob := publicHandle(t, root, "bob@example.com", dial)
	prev := folderLockWait
	folderLockWait = 100 * time.Millisecond
	t.Cleanup(func() { folderLockWait = prev })
	err := alice.HoldFolder("INBOX", "test", func() error {
		return bob.HoldFolder("INBOX", "test", func() error { return nil })
	})
	if err == nil {
		t.Fatal("bob held the public folder while alice held it")
	}
}
