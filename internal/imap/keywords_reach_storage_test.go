package imap_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// APPEND, COPY and a cross-namespace MOVE each hand one flag list to Save, and
// all three passed the system flags alone -- no keyword reached disk (#1783).

// keywordOnDisk returns the flag part of the single name in a maildir's cur/,
// and the folder's keyword file.
func keywordOnDisk(t *testing.T, folderPath string) (info, keywordFile string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(folderPath, "cur"))
	if err != nil {
		t.Fatalf("read %s/cur: %v", folderPath, err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d files in %s/cur, want 1", len(entries), folderPath)
	}
	name := entries[0].Name()
	info = name
	if i := strings.Index(name, ":2,"); i >= 0 {
		info = name[i+3:]
	}
	raw, err := os.ReadFile(filepath.Join(folderPath, "dovecot-keywords"))
	if err != nil {
		t.Fatalf("keyword file in %s: %v", folderPath, err)
	}
	return info, string(raw)
}

func appendWithKeyword(t *testing.T, c *imapclient.Client, mbox string) {
	t.Helper()
	body := []byte(testMsg)
	ac := c.Append(mbox, int64(len(body)), &imap.AppendOptions{
		Flags: []imap.Flag{imap.FlagSeen, imap.Flag("$Forwarded")},
	})
	if _, err := ac.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := ac.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Wait(); err != nil {
		t.Fatalf("APPEND: %v", err)
	}
}

func TestAppendKeywordReachesTheName(t *testing.T) {
	c, root := startTestServerAt(t)
	defer func() { c.Logout().Wait() }() //nolint:errcheck
	if err := c.Login("user@test.com", "testpass").Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}
	appendWithKeyword(t, c, "INBOX")

	info, kwFile := keywordOnDisk(t, filepath.Join(root, "test.com", "user", "Maildir"))
	if !strings.Contains(info, "S") {
		t.Errorf("flags in the name are %q, and APPEND set \\Seen", info)
	}
	if !strings.ContainsAny(info, "abcdefghijklmnopqrstuvwxyz") {
		t.Errorf("flags in the name are %q, and APPEND set $Forwarded", info)
	}
	if !strings.Contains(kwFile, "$Forwarded") {
		t.Errorf("the keyword file is %q, and APPEND set $Forwarded", kwFile)
	}
}

func TestCopiedKeywordReachesTheName(t *testing.T) {
	c, root := startTestServerAt(t)
	defer func() { c.Logout().Wait() }() //nolint:errcheck
	if err := c.Login("user@test.com", "testpass").Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}
	if err := c.Create("Target", nil).Wait(); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	appendWithKeyword(t, c, "INBOX")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if _, err := c.Copy(imap.SeqSetNum(1), "Target").Wait(); err != nil {
		t.Fatalf("COPY: %v", err)
	}

	info, kwFile := keywordOnDisk(t, filepath.Join(root, "test.com", "user", "Maildir", ".Target"))
	if !strings.ContainsAny(info, "abcdefghijklmnopqrstuvwxyz") {
		t.Errorf("flags in the copy's name are %q, and the source carried $Forwarded", info)
	}
	if !strings.Contains(kwFile, "$Forwarded") {
		t.Errorf("the target's keyword file is %q, and the source carried $Forwarded", kwFile)
	}
}

func TestMovedKeywordReachesTheNameAcrossNamespaces(t *testing.T) {
	aliceHome, dial := enforceServerWithShared(t)
	c := dial("alice")
	defer func() { c.Logout().Wait() }() //nolint:errcheck
	// Seen through the shared namespace the handles differ, so MOVE takes its
	// copy-the-body branch. Owner rights do not follow it there, hence the grant.
	if err := c.Create("Target", nil).Wait(); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	seedACL(t, aliceHome, "Target", "user=alice lrswipkxte\n")
	appendWithKeyword(t, c, "INBOX")
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if _, err := c.Move(imap.SeqSetNum(1), "Shared/Target").Wait(); err != nil {
		t.Fatalf("MOVE: %v", err)
	}

	info, kwFile := keywordOnDisk(t, filepath.Join(aliceHome, ".Target"))
	if !strings.ContainsAny(info, "abcdefghijklmnopqrstuvwxyz") {
		t.Errorf("flags in the moved name are %q, and the source carried $Forwarded", info)
	}
	if !strings.Contains(kwFile, "$Forwarded") {
		t.Errorf("the target's keyword file is %q, and the source carried $Forwarded", kwFile)
	}
}
