package imap

import (
	"path/filepath"
	"testing"

	fileindex "github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// NOTIFY hears a public folder on the store's key, by its name within the
// namespace, and a personal one on the user's (#2600); before, a public
// folder's key named the user and the full name, which no publisher used.
func TestNotifyKeysNameTheSharedStore(t *testing.T) {
	root := t.TempDir()
	public := filepath.Join(root, "public")
	personalSpec := NamespaceSpec{Type: NamespacePersonal, Prefix: "", Separator: '/', List: ListYes, Inbox: true}
	publicSpec := NamespaceSpec{Type: NamespaceShared, Prefix: "Public/", Separator: '/', List: ListYes, Location: "maildir:" + public}
	s := &session{srv: &Server{opts: Options{
		Mailbox: maildir.New(), Index: fileindex.New(), Namespaces: []NamespaceSpec{personalSpec, publicSpec},
	}}}
	s.userInfo = &mailbox.UserInfo{Username: "alice@example.com", Home: filepath.Join(root, "alice"), SessionID: "t1"}
	handles, _, err := s.openHandles(s.userInfo)
	if err != nil {
		t.Fatal(err)
	}
	s.namespaces = handles
	ks := s.notifyKeys()
	for name, want := range map[string]string{
		"News":        locks.MailboxKey("alice@example.com", "News"),
		"Public/News": locks.MailboxKey("store:"+public, "News"),
	} {
		if got := ks.folder(name); got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
	if got := ks.listNames(); len(got) != 2 || got[1] != "store:"+public {
		t.Errorf("list keys %v", got)
	}
}
