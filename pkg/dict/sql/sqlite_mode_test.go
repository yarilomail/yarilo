//go:build unix

package sql

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/dict"
)

// A new sqlite dict file is the owner's only, whatever the umask; an existing
// one keeps the mode the operator gave it (#2422).
func TestTheSQLiteDictFileMode(t *testing.T) {
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
	dir := t.TempDir()

	fresh := filepath.Join(dir, "fresh.db")
	d, err := New(dict.Config{Settings: map[string]any{"driver": "sqlite", "dsn": fresh}})
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	if got := mode(t, fresh); got != 0o600 {
		t.Errorf("new dict file mode = %o, want 600", got)
	}

	kept := filepath.Join(dir, "kept.db")
	if err := os.WriteFile(kept, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(kept, 0o640); err != nil {
		t.Fatal(err)
	}
	d, err = New(dict.Config{Settings: map[string]any{"driver": "sqlite", "dsn": kept}})
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	if got := mode(t, kept); got != 0o640 {
		t.Errorf("existing dict file mode = %o, want the operator's 640", got)
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}
