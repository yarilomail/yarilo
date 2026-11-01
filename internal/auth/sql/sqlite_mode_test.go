//go:build unix

package sql

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
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
			if got := fileMode(t, sqliteFilePath(dsn)); got != 0o600 {
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

func TestSQLiteFilePath(t *testing.T) {
	for _, tc := range []struct{ dsn, want string }{
		{"/var/lib/yarilo/users.db", "/var/lib/yarilo/users.db"},
		{"users.db?_pragma=foreign_keys(1)", "users.db"},
		{"file:/var/lib/yarilo/users.db", "/var/lib/yarilo/users.db"},
		{"file:///var/lib/yarilo/users.db?mode=rwc", "/var/lib/yarilo/users.db"},
		{"file://localhost/var/lib/my%20db.db", "/var/lib/my db.db"},
		{"file:/var/lib/yarilo/users.db?mode=ro", ""},
		{"file:/var/lib/yarilo/users.db?mode=rw", ""},
		{"file:mem?mode=memory", ""},
		{"file:/x.db?vfs=memdb", ""},
		{":memory:", ""},
		{"file::memory:", ""},
		{"", ""},
	} {
		if got := sqliteFilePath(tc.dsn); got != tc.want {
			t.Errorf("sqliteFilePath(%q) = %q, want %q", tc.dsn, got, tc.want)
		}
	}
}
