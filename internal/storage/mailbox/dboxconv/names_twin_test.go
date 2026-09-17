package dboxconv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two connections on one login both run the pass, and one may rename an
// ancestor between the other's walk and its rename. The path collected by the
// walk is then stale, and the pass must follow the folder, not fail (#1886).
func TestAPassFollowsAParentATwinRenamed(t *testing.T) {
	const (
		encodedParent = "&BBIERQRWBDQEPQRW-" // Вхідні
		encodedChild  = "&BCAEPgQxBD4EQgQw-" // Робота
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
