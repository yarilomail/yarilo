package imap

import (
	"path/filepath"
	"testing"

	fileindex "github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A session opens a public namespace's store under the store's lock name and
// its own under the user's: the keys every lock on the store is built from
// (#2598).
func TestOpenHandlesNamesTheSharedStore(t *testing.T) {
	root := t.TempDir()
	public := filepath.Join(root, "public")
	personalSpec := NamespaceSpec{Type: NamespacePersonal, Prefix: "", Separator: '/', List: ListYes, Inbox: true}
	publicSpec := NamespaceSpec{Type: NamespaceShared, Prefix: "Public/", Separator: '/', List: ListYes, Location: "maildir:" + public}
	s := &session{srv: &Server{opts: Options{
		Mailbox:    maildir.New(),
		Index:      fileindex.New(),
		Namespaces: []NamespaceSpec{personalSpec, publicSpec},
	}}}
	ui := &mailbox.UserInfo{Username: "alice@example.com", Home: filepath.Join(root, "alice"), SessionID: "t1"}
	s.userInfo = ui
	handles, _, err := s.openHandles(ui)
	if err != nil {
		t.Fatal(err)
	}
	for prefix, want := range map[string]string{"": "alice@example.com", "Public/": "store:" + public} {
		h := handles[prefix]
		if h == nil || h.box == nil {
			t.Fatalf("no handle for %q", prefix)
		}
		ln, ok := mailbox.Driver(h.box).(mailbox.LockNamer)
		if !ok || ln.LockUser() != want {
			t.Errorf("%q: lock name %v, want %q", prefix, ln, want)
		}
	}
}
