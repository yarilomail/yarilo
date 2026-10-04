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

// A folder deleted through the Box takes its index and identity with it, even
// where the index lives outside the mail, so the same name comes back new.
func TestDeleteTakesTheIndexOutsideTheMailWithIt(t *testing.T) {
	info := &mailbox.UserInfo{Username: "u@example.com", Home: t.TempDir(), Driver: "maildir",
		IndexDir: filepath.Join(t.TempDir(), "index")}
	store := maildir.New().OpenUser(info)
	idx := file.New().OpenUser(info)
	t.Cleanup(func() { _ = store.Close(); _ = idx.Close() })
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	box := mailboxbase.Open(store, idx)
	if err := store.Create("Gone"); err != nil {
		t.Fatal(err)
	}
	before, err := box.Folder("Gone", 0)
	if err != nil {
		t.Fatal(err)
	}
	dir := idx.(interface{ IndexDirFor(string) string }).IndexDirFor("Gone")
	ids := folders.New(mailbox.ControlRoot(info), info.Username, "", nil)
	if _, known, _ := ids.UIDValidity("Gone"); !known {
		t.Fatal("no identity record before the delete, so this row proves nothing")
	}

	if err := box.Delete("Gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the index directory %s survived the delete: %v", dir, err)
	}
	if v, known, _ := ids.UIDValidity("Gone"); known {
		t.Errorf("the identity record survived the delete with uidvalidity %d", v)
	}

	if err := store.Create("Gone"); err != nil {
		t.Fatal(err)
	}
	after, err := box.Folder("Gone", 0)
	if err != nil {
		t.Fatal(err)
	}
	if after.UIDValidity == before.UIDValidity || after.GUID == before.GUID {
		t.Errorf("recreated folder kept its identity: uidvalidity %d→%d, guid %x→%x",
			before.UIDValidity, after.UIDValidity, before.GUID, after.GUID)
	}
}
