package backendapi

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

var autoNS = []config.NamespaceConfig{{Type: "personal", Prefix: "", Separator: "/", List: "yes", Inbox: true,
	Mailboxes: map[string]config.NamespaceMailboxConfig{
		"Sent":  {Auto: "subscribe"},
		"Trash": {Auto: "create"},
	}}}

// The admin listing of a fresh account, with no IMAP or JMAP session before it,
// shows the configured mailboxes and their roles, and writes nothing.
func TestTheAdminListingShowsConfiguredMailboxesAndMakesNothing(t *testing.T) {
	ts, root := storageTestServer(t, func(o *Options) { o.Namespaces = autoNS })
	const user = "alice@example.com"
	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "", map[string]any{"user": user})
	if status != http.StatusOK {
		t.Fatalf("folder/list status=%d body=%s", status, body)
	}
	var got struct {
		Folders    []string          `json:"folders"`
		NotCreated []string          `json:"not_created"`
		SpecialUse map[string]string `json:"special_use"`
	}
	decodeJSONBody(t, body, &got)
	for _, name := range []string{"Sent", "Trash"} {
		if !slices.Contains(got.Folders, name) || !slices.Contains(got.NotCreated, name) {
			t.Fatalf("%s is not listed as configured and not created: %+v", name, got)
		}
	}
	if got.SpecialUse["Sent"] != `\Sent` {
		t.Fatalf("Sent lists with special use %q", got.SpecialUse["Sent"])
	}
	home := filepath.Join(root, "example.com", "alice")
	if _, err := os.Stat(filepath.Join(home, "Maildir", ".Sent")); !os.IsNotExist(err) {
		t.Fatalf("the listing made Sent: %v", err)
	}
}

// folder/info answers for a configured mailbox before any server made it, and
// with its real identity after the first IMAP LIST did.
func TestFolderInfoOfAConfiguredMailboxBeforeAndAfterItIsMade(t *testing.T) {
	ts, root := storageTestServer(t, func(o *Options) { o.Namespaces = autoNS })
	const user = "alice@example.com"
	info := func() map[string]any {
		status, body := doJSON(t, ts, http.MethodPost, "/api/backend/folder/info", "", map[string]any{"user": user, "folder": "Sent"})
		if status != http.StatusOK {
			t.Fatalf("folder/info status=%d body=%s", status, body)
		}
		var out map[string]any
		decodeJSONBody(t, body, &out)
		return out
	}
	// The home must exist for the info path; INBOX is what any login makes.
	resolver := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}
	ui := resolver.UserInfo(user, "")
	store := mailbox.Validating(maildir.New(), mailbox.DefaultNameRules()).OpenUser(ui)
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	if before := info(); before["created"] != false || before["configured"] != true || before["guid"] != nil {
		t.Fatalf("before any open: %v", before)
	}
	idx := file.New().OpenUser(ui)
	box := mailboxbase.Open(store, idx, mailboxbase.WithAuto(mailboxbase.Auto{Mailboxes: autoNS[0].AutoMailboxes()}))
	if _, err := box.ListFolders(); err != nil {
		t.Fatal(err)
	}
	_ = idx.Close()
	_ = store.Close()
	if after := info(); after["created"] != true || after["guid"] == "" || after["guid"] == nil {
		t.Fatalf("after the first LIST: %v", after)
	}
}
