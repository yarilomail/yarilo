package imap_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// findUnder returns the paths under root whose base name is name.
func findUnder(t *testing.T, root, name string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Name() == name {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// A selected mailbox whose UIDVALIDITY changes under the session ends it: every
// uid the client holds names another message now (#2083).
func TestASessionWhoseMailboxChangesUIDValidityIsDisconnected(t *testing.T) {
	root := t.TempDir()
	c := startServerWithRoot(t, maildirBackend(t), root)
	msg := "From: a@b\r\nSubject: s\r\n\r\nx\r\n"
	app := c.Append("INBOX", int64(len(msg)), nil)
	if _, err := app.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}

	// Another generation of the list, and an arrival so the next poll walks.
	lists := findUnder(t, root, "yarilo-uidlist")
	if len(lists) != 1 {
		t.Fatalf("found %d lists, want INBOX's alone: %v", len(lists), lists)
	}
	data, err := os.ReadFile(lists[0])
	if err != nil {
		t.Fatal(err)
	}
	other := regexp.MustCompile(`V\d+`).ReplaceAll(data, []byte("V12345"))
	if err := os.WriteFile(lists[0], other, 0o600); err != nil {
		t.Fatal(err)
	}
	newDir := filepath.Join(filepath.Dir(lists[0]), "new")
	if err := os.WriteFile(filepath.Join(newDir, "1700000009.M9P9.host"), []byte(msg), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := c.Noop().Wait(); err == nil {
		t.Fatal("the session kept serving a mailbox whose UIDVALIDITY changed under it")
	}
}

// Deleting the selected mailbox closes it first: the session goes on, and no
// poll brings the deleted folder's index back (#2084).
func TestDeletingTheSelectedMailboxDoesNotBringItBack(t *testing.T) {
	root := t.TempDir()
	c := startServerWithRoot(t, maildirBackend(t), root)
	if err := c.Create("Gone", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select("Gone", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete("Gone").Wait(); err != nil {
		t.Fatalf("DELETE of the selected mailbox: %v", err)
	}
	if err := c.Noop().Wait(); err != nil {
		t.Fatalf("the session ended after deleting its selected mailbox: %v", err)
	}
	for _, p := range findUnder(t, root, "yarilo.index") {
		if strings.Contains(p, "Gone") {
			t.Errorf("the deleted mailbox's index is back: %s", p)
		}
	}
}
