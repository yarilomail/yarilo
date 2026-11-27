package file

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// createFreshLevel opens INBOX in a new handle on root with every level
// captured and returns the level of the createFresh line, or "" for none.
func createFreshLevel(t *testing.T, root string) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	u := openIdx(root, testUser)
	_, err := u.OpenFolder("INBOX", 0, "")
	slog.SetDefault(prev)
	if err != nil {
		t.Fatal(err)
	}
	_ = u.Close()
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, "createFresh resetting NextUID") && strings.Contains(l, "folder=INBOX") {
			i := strings.Index(l, "level=")
			return strings.Fields(l[i+len("level="):])[0]
		}
	}
	return ""
}

// A folder opened for the first time has no past: its fresh index is a Debug
// line. A folder the identity record knows had an index; a fresh one for it
// resets an established folder and stays a WARN.
func TestAFreshIndexIsWarnOnlyForAFolderThatHadOne(t *testing.T) {
	root := t.TempDir()
	if got := createFreshLevel(t, root); got != "DEBUG" {
		t.Errorf("first open of a new folder logged at %q, want DEBUG", got)
	}
	u := openIdx(root, testUser)
	dir := u.indexDir("INBOX")
	_ = u.Close()
	gone, err := filepath.Glob(filepath.Join(dir, "yarilo.index*"))
	if err != nil || len(gone) == 0 {
		t.Fatalf("no index files under %s to remove: %v", dir, err)
	}
	for _, f := range gone {
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}
	baseImages.Delete(indexPathFor(dir))
	if got := createFreshLevel(t, root); got != "WARN" {
		t.Errorf("a fresh index for a folder that had one logged at %q, want WARN", got)
	}
}
