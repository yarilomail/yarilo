package backendapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/userstate/acl"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/dict"
	_ "github.com/yarilomail/yarilo/pkg/dict/memory"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// An operator asking who granted this user is answered from the registry, not
// from the interval cache a session's LIST reads: a grant made after a session
// was answered must be visible to the operator before it is visible to LIST.
func TestACLRegistryListReadsPastTheListInterval(t *testing.T) {
	root := t.TempDir()
	d, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })

	s := New(Options{
		Dicts:      map[string]dict.Dict{"metadata": d},
		Mailbox:    mailbox.Validating(maildir.New(), mailbox.DefaultNameRules()),
		Index:      file.New(),
		Resolver:   &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"},
		Namespaces: []config.NamespaceConfig{{Type: "personal", Prefix: "", Separator: "/", List: "yes", Inbox: true}},
		SharedDict: d,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	const seer = "bob@example.com"

	// A session's LIST asks first and is answered: nobody has granted bob
	// anything, and that answer is now held for the interval.
	if owners, oerr := acl.OwnersFor(context.Background(), d, seer, nil); oerr != nil || len(owners) != 0 {
		t.Fatalf("the first lookup answered %v (err=%v), want nothing", owners, oerr)
	}

	// A grant lands from somewhere this process did not serve -- another
	// backend, or the rebuild verb -- so nothing here invalidates.
	tx, err := d.Begin(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Set("shared/shared-boxes/user/"+dict.Escape(seer)+"/alice@example.com", []byte("1")); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/acl/registry/list", "",
		map[string]any{"user": seer})
	if status != 200 {
		t.Fatalf("registry list: status=%d body=%s", status, body)
	}
	if !strings.Contains(string(body), "alice@example.com") {
		t.Errorf("the admin answer is the cached one, not the registry: %s", body)
	}

	// And LIST still holds its answer, which is the behaviour the page
	// documents: the two surfaces differ on purpose.
	if owners, oerr := acl.OwnersFor(context.Background(), d, seer, nil); oerr != nil || len(owners) != 0 {
		t.Errorf("LIST no longer holds its answer: %v (err=%v)", owners, oerr)
	}
}
