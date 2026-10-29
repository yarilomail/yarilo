package dboxconv_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/dboxconv"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/mdbox/mdboxmap"
)

// A missing tree is empty; an unreadable one is an error, not an empty answer
// that would let their map be dropped while folders still need it.
func TestATreeThatCannotBeReadIsNotAnEmptyOne(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "mailboxes")
	if n, err := dboxconv.AdoptNames(missing, true); err != nil || n != 0 {
		t.Errorf("adopt in a missing tree: %d, %v", n, err)
	}
	if left, err := dboxconv.AnyForeignFolderLeft(missing); err != nil || left {
		t.Errorf("foreign folders in a missing tree: %v, %v", left, err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads through file modes")
	}
	tree := filepath.Join(t.TempDir(), "mailboxes")
	locked := filepath.Join(tree, "Work")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if _, err := dboxconv.AdoptNames(tree, true); err == nil {
		t.Error("adopted names over a tree it could not read")
	}
	if _, err := dboxconv.AnyForeignFolderLeft(tree); err == nil {
		t.Error("answered no foreign folder left over a tree it could not read")
	}
	storage := t.TempDir()
	mapLog := filepath.Join(storage, "dovecot.map.index.log")
	if err := os.WriteFile(mapLog, []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	ours, err := mdboxmap.Open(t.TempDir(), "u")
	if err != nil {
		t.Fatal(err)
	}
	defer ours.Close() //nolint:errcheck
	if dropped, err := dboxconv.DropForeignMapIfDone(storage, tree, t.TempDir(), ours); err == nil || dropped {
		t.Errorf("dropped=%v err=%v over a tree it could not read", dropped, err)
	}
	if _, err := os.Stat(mapLog); err != nil {
		t.Errorf("their map went with the tree unread: %v", err)
	}
}

// A name the disk will not let be renamed is an error, and the folder keeps the
// name it had: a half-adopted tree is not reported as done.
func TestANameTheDiskWillNotRenameIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root renames through file modes")
	}
	tree := filepath.Join(t.TempDir(), "mailboxes")
	encoded := filepath.Join(tree, "&BBIERQRWBDQEPQRW-") // modified UTF-7 for "Вхідні"
	if err := os.MkdirAll(encoded, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tree, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tree, 0o700) })
	if n, err := dboxconv.AdoptNames(tree, true); err == nil {
		t.Errorf("adopted %d names in a tree the disk would not rename in", n)
	}
	if _, err := os.Stat(encoded); err != nil {
		t.Errorf("the folder lost its name: %v", err)
	}
}
