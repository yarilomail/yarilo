//go:build unix

package sql

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/sqlitefile"
)

// withUmask runs the test under a typical host umask, which on its own
// would create the database 0644.
func withUmask(t *testing.T, mask int) {
	t.Helper()
	old := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(old) })
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

func TestNewCreatesSQLiteFile0600(t *testing.T) {
	withUmask(t, 0o022)
	dir := t.TempDir()
	for name, dsn := range map[string]string{
		"plain path": filepath.Join(dir, "plain.db"),
		"file URI":   "file:" + filepath.Join(dir, "uri.db") + "?_pragma=busy_timeout(1000)",
	} {
		t.Run(name, func(t *testing.T) {
			p, err := New(Config{Driver: "sqlite", DSN: dsn})
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			if got := fileMode(t, sqlitefile.Path(dsn)); got != 0o600 {
				t.Fatalf("mode = %o, want 600", got)
			}
		})
	}
}

func TestNewKeepsExistingSQLiteMode(t *testing.T) {
	withUmask(t, 0o022)
	path := filepath.Join(t.TempDir(), "users.db")
	if err := os.WriteFile(path, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	p, err := New(Config{Driver: "sqlite", DSN: path})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if got := fileMode(t, path); got != 0o640 {
		t.Fatalf("mode = %o, want the operator's 640", got)
	}
}
