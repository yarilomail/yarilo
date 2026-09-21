package file

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// foreignStore lays out an mdbox store the way a reference install wrote it:
// their folder log and cache under the index root, their map and storage file
// beside it. The files are that install's own (testdata/foreign-cache).
func foreignStore(t *testing.T) *mailbox.UserInfo {
	t.Helper()
	home := t.TempDir()
	layout := map[string]string{
		"dovecot.index.log":     filepath.Join("index", "mailboxes", "Fixture", "dovecot.index.log"),
		"dovecot.index.cache":   filepath.Join("index", "mailboxes", "Fixture", "dovecot.index.cache"),
		"dovecot.map.index":     filepath.Join("index", "storage", "dovecot.map.index"),
		"dovecot.map.index.log": filepath.Join("index", "storage", "dovecot.map.index.log"),
		"m.1":                   filepath.Join("mdbox", "storage", "m.1"),
	}
	for src, rel := range layout {
		b, err := os.ReadFile(filepath.Join("testdata", "foreign-cache", src))
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &mailbox.UserInfo{
		Username: "u1@d00001.test",
		Home:     home,
		Driver:   "mdbox",
		IndexDir: filepath.Join(home, "index"),
	}
}

// Their cache comes with the folder: it is not deleted, its records are
// checksummed in one pass, and every record that points into it carries a
// non-zero checksum from the first read (#1714).
func TestAConvertedFolderKeepsTheirCache(t *testing.T) {
	info := foreignStore(t)
	idx := New().OpenUser(info)
	defer idx.Close() //nolint:errcheck

	f, err := idx.OpenFolder("Fixture", 1)
	if err != nil {
		t.Fatalf("open the converted folder: %v", err)
	}
	msgs, err := idx.GetMessages(f.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 {
		t.Fatal("the folder converted empty, so nothing is asserted")
	}

	// Our index for an mdbox folder lives under dbox-Mails; theirs sat one
	// level up, beside their own index.
	theirDir := filepath.Join(info.Home, "index", "mailboxes", "Fixture")
	indexDir := filepath.Join(theirDir, "dbox-Mails")
	if _, err := os.Stat(filepath.Join(indexDir, mailindex.CacheFileName)); err != nil {
		t.Fatalf("their cache was not adopted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(theirDir, "dovecot.index.cache")); !os.IsNotExist(err) {
		t.Error("their cache file is still there under its own name")
	}

	var stamped int
	for _, m := range msgs {
		if m.CacheOffset == 0 {
			continue
		}
		stamped++
		if m.CacheCRC == 0 {
			t.Errorf("uid %d points at cache offset %d with no checksum", m.UID, m.CacheOffset)
		}
	}
	if stamped == 0 {
		t.Fatal("no record points into the adopted cache, so the checksums prove nothing")
	}
}
