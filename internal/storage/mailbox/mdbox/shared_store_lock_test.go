package mdbox

import (
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/mdbox/mdboxmap"
	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/locks/lockstest"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

func publicMdbox(t *testing.T, root, user string, dial func() locks.Locker) *userMailbox {
	t.Helper()
	base := &mailbox.UserInfo{Username: user, Home: t.TempDir()}
	loc, ok, err := mailbox.ParseLocation("mdbox:"+root, base)
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

// Two users on one public mdbox store exclude each other on the folder and on
// the map: while one holds either, the other's take waits and gives up
// (#2598). Two map writers at once is the race #2234 closed for one user.
func TestTwoUsersOnOnePublicStoreExcludeEachOther(t *testing.T) {
	dial := lockstest.Server(t)
	root := t.TempDir()
	alice := publicMdbox(t, root, "alice@example.com", dial)
	bob := publicMdbox(t, root, "bob@example.com", dial)
	prevF, prevM := folderLockWait, mdboxmap.MapLockWait
	folderLockWait, mdboxmap.MapLockWait = 100*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { folderLockWait, mdboxmap.MapLockWait = prevF, prevM })

	t.Run("folder", func(t *testing.T) {
		err := alice.HoldFolder("INBOX", "test", func() error {
			return bob.HoldFolder("INBOX", "test", func() error { return nil })
		})
		if err == nil {
			t.Fatal("bob held the public folder while alice held it")
		}
	})
	t.Run("map", func(t *testing.T) {
		am, err := alice.openMap()
		if err != nil {
			t.Fatal(err)
		}
		err = am.Place(func(*mdboxmap.Placement) error {
			_, _, _, serr := bob.Save("INBOX", strings.NewReader("Subject: s\r\n\r\nx\r\n"), 0, 17, nil, nil, [16]byte{})
			return serr
		})
		if err == nil {
			t.Fatal("bob wrote the public map while alice held it")
		}
	})
}
