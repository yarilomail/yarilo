package mailboxbase_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	fileidx "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

var errReopen = errors.New("index gone")

// failingReopen is an index whose second OpenFolder fails, which is the reopen
// a settled folder makes.
type failingReopen struct {
	mailbox.UserIndex
	opens int
}

func (f *failingReopen) OpenFolder(name string, uidValidity uint32) (*mailbox.Folder, error) {
	f.opens++
	if f.opens > 1 {
		return nil, errReopen
	}
	return f.UserIndex.OpenFolder(name, uidValidity)
}

// A reopen that fails leaves the caller with a handle describing a folder that
// has moved on, so the open refuses rather than answering with a stale UIDNEXT.
func TestAFailedReopenAfterSettlingRefuses(t *testing.T) {
	root := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x.com", Home: filepath.Join(root, "x.com", "u")}
	store := maildir.New().OpenUser(info)
	t.Cleanup(func() { store.Close() }) //nolint:errcheck
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	if err := store.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	inner := fileidx.New().OpenUser(info)
	t.Cleanup(func() { inner.Close() }) //nolint:errcheck
	if _, err := inner.OpenFolder("INBOX", 1); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mailboxbase.SetTestSyncTokens(8))

	inbox := filepath.Join(info.Home, "Maildir")
	deliverOutOfBand(t, inbox, "1700000001.M1P1_1.host,S=20,W=20:2,", time.Now().Add(-time.Hour))

	idx := &failingReopen{UserIndex: inner}
	box := mailboxbase.Open(store, idx)
	f, err := box.Folder("INBOX", 1)
	if !errors.Is(err, errReopen) {
		t.Fatalf("Folder returned %v, want the reopen failure", err)
	}
	if f != nil {
		t.Errorf("Folder handed back a folder as well as the error: %+v", f)
	}
}
