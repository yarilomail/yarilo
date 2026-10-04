package dboxconv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A twin pass may rename an ancestor between this walk and this rename: the
// collected path is then stale, and the pass must follow the folder (#1886).
func TestAPassFollowsAParentATwinRenamed(t *testing.T) {
	const (
		encodedParent = "&BBIERQRWBDQEPQRW-" // modified UTF-7 for "Вхідні"
		encodedChild  = "&BCAEPgQxBD4EQgQw-" // modified UTF-7 for "Робота"
		parent        = "Вхідні"
		child         = "Робота"
	)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, encodedParent, encodedChild), 0o700); err != nil {
		t.Fatal(err)
	}

	// No sleeps: the twin's rename happens while this pass sits in the hook.
	twinDone := make(chan error, 1)
	fired := false
	beforeRename = func(dir string) {
		if fired || filepath.Base(dir) != encodedChild {
			return
		}
		fired = true
		go func() {
			twinDone <- os.Rename(filepath.Join(root, encodedParent), filepath.Join(root, parent))
		}()
		if err := <-twinDone; err != nil {
			t.Errorf("the twin could not rename the parent: %v", err)
		}
	}
	t.Cleanup(func() { beforeRename = func(string) {} })

	if _, err := AdoptNames(root, true); err != nil {
		t.Fatalf("the pass failed after a twin renamed the parent: %v", err)
	}
	if !fired {
		t.Fatal("the hook never fired, so nothing was interleaved")
	}

	if _, err := os.Stat(filepath.Join(root, parent, child)); err != nil {
		t.Errorf("the child did not follow its parent: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != parent {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the tree holds %s, want exactly %s", strings.Join(names, ","), parent)
	}
}

// The other moment: a twin renames the ancestor after this pass's rename and
// before it is made durable. The path the directory was opened by then names
// nothing, and a rename that succeeded used to report ENOENT (#1938).
func TestAPassSurvivesATwinRenamingTheParentAfterTheRename(t *testing.T) {
	const (
		encodedParent = "&BBIERQRWBDQEPQRW-"
		encodedChild  = "&BCAEPgQxBD4EQgQw-"
		parent        = "Вхідні"
		child         = "Робота"
	)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, encodedParent, encodedChild), 0o700); err != nil {
		t.Fatal(err)
	}

	fired := false
	afterRename = func(src string) {
		if fired || filepath.Base(src) != encodedChild {
			return
		}
		fired = true
		done := make(chan error, 1)
		go func() {
			done <- os.Rename(filepath.Join(root, encodedParent), filepath.Join(root, parent))
		}()
		if err := <-done; err != nil {
			t.Errorf("the twin could not rename the parent: %v", err)
		}
	}
	t.Cleanup(func() { afterRename = func(string) {} })

	if _, err := AdoptNames(root, true); err != nil {
		t.Fatalf("the pass failed after a twin renamed the parent under a finished rename: %v", err)
	}
	if !fired {
		t.Fatal("the hook never fired, so nothing was interleaved")
	}
	if _, err := os.Stat(filepath.Join(root, parent, child)); err != nil {
		t.Errorf("the child is not where the rename left it: %v", err)
	}
}
