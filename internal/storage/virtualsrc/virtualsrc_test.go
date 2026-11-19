package virtualsrc_test

import (
	"context"
	"path/filepath"
	"testing"

	fileindex "github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/internal/storage/nsopen"
	"github.com/yarilomail/yarilo/v2/internal/storage/virtualsrc"
	"github.com/yarilomail/yarilo/v2/internal/userstate/acl"
	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/dict"
	_ "github.com/yarilomail/yarilo/v2/pkg/dict/memory"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// An owner-templated namespace gives a virtual mailbox a store per owner who
// granted the user something, named by the owner's prefix, and none for an
// owner who did not (#2603 rule 2).
func TestAnOwnerTemplatedNamespaceGivesAStorePerGrantingOwner(t *testing.T) {
	for _, typ := range []string{"shared", "other"} {
		t.Run(typ, func(t *testing.T) { ownerTemplated(t, typ) })
	}
}

// ownerTemplated is the row for one namespace type: the documented Other Users
// form is type other with an owner template.
func ownerTemplated(t *testing.T, typ string) {
	root := t.TempDir()
	d, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	grant := []acl.ListEntry{{Mailbox: "Shared", Identifier: mailbox.Identifier{Type: mailbox.IDUser, Name: "bob@x"}, Rights: "lr"}}
	if err := acl.NewRegistry(d, "alice@x").SyncFromList(grant, true); err != nil {
		t.Fatal(err)
	}
	acl.InvalidateAll()
	homes := map[string]string{"alice@x": filepath.Join(root, "alice"), "carol@x": filepath.Join(root, "carol")}
	lookup := func(_ context.Context, owner string) (*mailbox.UserInfo, error) {
		return &mailbox.UserInfo{Username: owner, Home: homes[owner], MailPath: filepath.Join(homes[owner], "Maildir"), Driver: "maildir"}, nil
	}
	namespaces := []config.NamespaceConfig{
		{Type: "personal", Prefix: "", Separator: "/"},
		{Type: typ, Prefix: "user/%u/", Separator: "/", Location: "maildir:%h/Maildir"},
		{Type: "other", Prefix: "others/", Separator: "/", Location: "maildir:/srv/others"},
	}
	sources := virtualsrc.Sources(namespaces, virtualsrc.Deps{
		Backends: nsopen.Backends{Mailbox: maildir.New()}, Index: fileindex.New(),
		SharedDict: d, OwnerLookup: lookup,
	})
	bob := &mailbox.UserInfo{Username: "bob@x", Home: filepath.Join(root, "bob")}
	got := sources(bob, "user/%u/")
	if len(got) != 1 || got[0].Prefix != "user/alice@x/" || got[0].Namespace != "user/%u/" {
		t.Fatalf("sources = %+v, want one store for alice under user/alice@x/", got)
	}
	decl := virtualsrc.Declared(namespaces)
	if len(decl) != 2 || !decl[0].OwnerTemplated || !decl[0].HasStore || decl[1].HasStore {
		t.Errorf("declared = %+v, want the owner template with a store and others/ without, as a session opens none there", decl)
	}
}
