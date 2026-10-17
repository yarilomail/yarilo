package mdbox

import (
	"os"
	"path/filepath"
	"testing"
)

// The FTS index of a converted store, with the mail directories an earlier
// release made inside it as on the sandbox's conv8, is no folder (#2254).
func TestTheFTSIndexDirectoryIsNotListed(t *testing.T) {
	mb, home := newTestUser(t)
	inbox := filepath.Join(home, "mdbox", "mailboxes", "INBOX")
	for _, d := range []string{
		"fts-flatcurve/dbox-Mails",
		"fts-flatcurve/current.1788123078618997/dbox-Mails",
		"fts-flatcurve/index.4028",
		"Receipts/dbox-Mails",
	} {
		if err := os.MkdirAll(filepath.Join(inbox, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	folders, err := mb.ListFolders()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range folders {
		got[f.Name] = true
	}
	if !got["INBOX"] || !got["INBOX/Receipts"] {
		t.Fatalf("ListFolders = %v, want INBOX and its real child INBOX/Receipts", folders)
	}
	for name := range got {
		if name != "INBOX" && name != "INBOX/Receipts" {
			t.Errorf("ListFolders lists %q: the FTS index directory and its contents are no folders", name)
		}
	}
}
