package v1legacy

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A damaged legacy file is refused with the damage named, never migrated as a
// shorter message.
func TestADamagedLegacyFileIsRefused(t *testing.T) {
	for name, damage := range map[string]func([]byte) []byte{
		"header type":       func(b []byte) []byte { b[3] = 'X'; return b },
		"size not hex":      func(b []byte) []byte { copy(b[14:30], "zzzzzzzzzzzzzzzz"); return b },
		"body past the end": func(b []byte) []byte { return b[:headerLen+3] },
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, "INBOX")
			writeV1File(t, dir, "u.1", []byte("hello world"), randomGUID(), 1700000000)
			p := filepath.Join(dir, "u.1")
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, damage(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadMessage(home, "INBOX", "u.1"); err == nil {
				t.Error("read a damaged file")
			}
		})
	}
	if _, err := ReadMessage(t.TempDir(), "INBOX", "u.9"); err == nil {
		t.Error("read a file that is not there")
	}
}

// Only u.* files are messages, in name order; a stray file or directory is not
// migrated, and a home that is not there is an error, not an empty account.
func TestOnlyMessageFilesAreListedInOrder(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "INBOX")
	for _, n := range []string{"u.3", "u.1", "u.2"} {
		writeV1File(t, dir, n, []byte("m"), randomGUID(), 1700000000)
	}
	if err := os.WriteFile(filepath.Join(dir, "dbox.index"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "u.dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "stray"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	msgs, err := ListMessages(home, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(msgs, []string{"u.1", "u.2", "u.3"}) {
		t.Errorf("listed %v", msgs)
	}
	if folders, err := ListFolders(home); err != nil || !slices.Equal(folders, []string{"INBOX"}) {
		t.Errorf("folders %v, %v", folders, err)
	}
	if _, err := ListFolders(filepath.Join(home, "nowhere")); err == nil {
		t.Error("listed a home that is not there")
	}
}
