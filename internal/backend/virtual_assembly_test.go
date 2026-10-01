package backend

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/virtual"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The virtual namespace the assembler builds draws on the personal mail it was
// given: no protocol has to hand it over (#1805).
func TestTheAssembledVirtualNamespaceHasItsFolders(t *testing.T) {
	root := t.TempDir()
	personal := (&mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}).UserInfo("alice@example.com", "")
	mb, idx := maildir.New(), file.New()
	box := mailboxbase.Open(mb.OpenUser(personal), idx.OpenUser(personal), mailboxbase.SaveOnly())
	if err := box.Store().Init(); err != nil {
		t.Fatal(err)
	}
	f, err := box.Folder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	raw := "Subject: x\r\n\r\nbody\r\n"
	name, vsize, guid, err := box.Store().Save("INBOX", strings.NewReader(raw), 1, int64(len(raw)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if err := box.RecordSaved(f, "INBOX", name, &mailbox.MessageMeta{Size: uint32(len(raw)), VSize: vsize, GUID: guid}); err != nil {
		t.Fatal(err)
	}
	box.Close()

	dir := filepath.Join(personal.Home, "virtual", "All")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, virtual.ConfigFileName), []byte("INBOX\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const location = "virtual:%h/virtual"
	ns, err := BuildNamespaceMailboxes([]config.NamespaceConfig{
		{Type: "personal", Prefix: "", Separator: "/", Inbox: true},
		{Type: "personal", Prefix: "Virtual/", Separator: "/", Location: location},
	}, "maildir", config.StorageConfig{}, nil, VirtualDeps{Mailbox: mb, Index: idx})
	if err != nil {
		t.Fatal(err)
	}
	loc, _, err := mailbox.ParseLocation(location, personal)
	if err != nil {
		t.Fatal(err)
	}
	vui, err := mailbox.NamespaceUserInfo(personal, loc, "/")
	if err != nil {
		t.Fatal(err)
	}
	vbox := mailboxbase.Open(ns["Virtual/"].OpenUser(vui), idx.OpenUser(vui))
	defer vbox.Close()
	all, err := vbox.Folder("All", 0)
	if err != nil {
		t.Fatalf("the assembled virtual namespace could not open its mailbox: %v", err)
	}
	if all.Messages != 1 {
		t.Errorf("the virtual mailbox holds %d messages, want the 1 in INBOX", all.Messages)
	}
}
