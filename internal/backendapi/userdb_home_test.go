package backendapi

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
)

// Every operator command resolves through one entry point, and it must open
// the mailbox a session opens: the userdb home, not the template's (#2024).
func TestOperatorCommandsOpenTheUserdbHome(t *testing.T) {
	root := t.TempDir()
	userdbHome := filepath.Join(root, "store", "shared-home")
	templateHome := filepath.Join(root, "example.com", "alice")

	udb := &stubIteratorUserdb{users: map[string]*protocol.UserInfo{
		"alice@example.com": {
			Username: "alice@example.com", UID: 1001, GID: 1001,
			// "~" is what the stand's userdb says, and it is expanded
			// against the home the resolver was given.
			Home: userdbHome, MailLocation: "maildir:~/Maildir",
		},
	}}
	ts := ownerTemplatedServer(t, root, udb)

	// One folder in each home, so the answer says which one was opened.
	for dir, folder := range map[string]string{
		filepath.Join(userdbHome, "Maildir"):   ".TheUserdbHome",
		filepath.Join(templateHome, "Maildir"): ".TheTemplateHome",
	} {
		for _, sub := range []string{"cur", "new", "tmp"} {
			if err := os.MkdirAll(filepath.Join(dir, folder, sub), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}

	status, body := doJSON(t, ts, http.MethodPost, "/api/backend/folder/list", "",
		map[string]any{"user": "alice@example.com"})
	if status != http.StatusOK {
		t.Fatalf("folder/list: HTTP %d: %s", status, body)
	}
	got := string(body)
	if !strings.Contains(got, "TheUserdbHome") {
		t.Errorf("the listing opened another mailbox than a session would: %s", got)
	}
	if strings.Contains(got, "TheTemplateHome") {
		t.Errorf("the listing came from the template home: %s", got)
	}
}
