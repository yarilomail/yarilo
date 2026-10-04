package mailboxbase_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/internal/userstate/folders"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A folder renamed through the Box takes its index and identity to the new
// name, even where the index lives outside the mail, and leaves nothing behind.
func TestRenameMovesTheIndexOutsideTheMailWithIt(t *testing.T) {
	info := &mailbox.UserInfo{Username: "u@example.com", Home: t.TempDir(), Driver: "maildir",
		IndexDir: filepath.Join(t.TempDir(), "index")}
	store := maildir.New().OpenUser(info)
	idx := file.New().OpenUser(info)
	t.Cleanup(func() { _ = store.Close(); _ = idx.Close() })
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	box := mailboxbase.Open(store, idx)
	if err := store.Create("Work"); err != nil {
		t.Fatal(err)
	}
	before, err := box.Folder("Work", 0)
	if err != nil {
		t.Fatal(err)
	}
	dirOf := idx.(interface{ IndexDirFor(string) string }).IndexDirFor
	ids := folders.New(mailbox.ControlRoot(info), info.Username, "", nil)

	if err := box.Rename("Work", "Play"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dirOf("Play"), file.IndexFileName)); err != nil {
		t.Errorf("no index under the new name: %v", err)
	}
	if _, err := os.Stat(dirOf("Work")); !os.IsNotExist(err) {
		t.Errorf("the index directory stayed under the old name: %v", err)
	}
	if v, known, _ := ids.UIDValidity("Play"); !known || v != before.UIDValidity {
		t.Errorf("identity under the new name: %d known=%v, want %d", v, known, before.UIDValidity)
	}
	if v, known, _ := ids.UIDValidity("Work"); known {
		t.Errorf("identity record stayed under the old name with uidvalidity %d", v)
	}
}
