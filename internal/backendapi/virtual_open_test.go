package backendapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/backend"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/virtual"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A virtual mailbox opened with no IMAP session at all shows what its folders
// hold now: the store follows them itself, whoever opens it (#1805).
func TestAVirtualMailboxOpenedWithoutIMAPFollowsItsFolders(t *testing.T) {
	root := t.TempDir()
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}
	const user = "alice@example.com"
	info, _ := resolver.UserInfo(user, "")
	mb, idx := maildir.New(), file.New()
	deliver := func(subject string) {
		t.Helper()
		box := mailboxbase.Open(mb.OpenUser(info), idx.OpenUser(info), mailboxbase.SaveOnly())
		defer box.Close()
		if err := box.Store().Init(); err != nil {
			t.Fatal(err)
		}
		f, err := box.Folder("INBOX", 0)
		if err != nil {
			t.Fatal(err)
		}
		raw := "Subject: " + subject + "\r\n\r\nbody\r\n"
		name, vsize, guid, err := box.Store().Save("INBOX", strings.NewReader(raw), 1, int64(len(raw)), nil, nil, [16]byte{})
		if err != nil {
			t.Fatal(err)
		}
		if err := box.RecordSaved(f, "INBOX", name, &mailbox.MessageMeta{Size: uint32(len(raw)), VSize: vsize, GUID: guid}); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(info.Home, "virtual", "All")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, virtual.ConfigFileName), []byte("INBOX\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	namespaces := []config.NamespaceConfig{
		{Type: "personal", Prefix: "", Separator: "/", List: "yes", Inbox: true},
		{Type: "personal", Prefix: "Virtual/", Separator: "/", List: "no", Location: "virtual:%h/virtual"},
	}
	ns, err := backend.BuildNamespaceMailboxes(namespaces, "maildir", config.StorageConfig{}, nil, backend.VirtualDeps{Mailbox: mb, Index: idx})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(Options{
		Mailbox: mb, Index: idx, Resolver: resolver, Namespaces: namespaces, NamespaceMailboxes: ns,
	}).Handler())
	t.Cleanup(ts.Close)
	count := func() any {
		t.Helper()
		_, raw := doJSON(t, ts, http.MethodPost, "/api/backend/folder/info", "",
			map[string]any{"user": user, "namespace": "virtual", "folder": "All"})
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode: %v: %s", err, raw)
		}
		return out["messages"]
	}

	deliver("first")
	if got := count(); got != float64(1) {
		t.Fatalf("the virtual mailbox counts %v, want the 1 message in INBOX", got)
	}
	deliver("second")
	if got := count(); got != float64(2) {
		t.Errorf("after a second delivery the virtual mailbox counts %v, want 2", got)
	}
}
