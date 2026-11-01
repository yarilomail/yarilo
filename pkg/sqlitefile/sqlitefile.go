// Package sqlitefile creates a SQLite database file with its mode set, ahead of
// the driver, which would leave the mode to the umask.
package sqlitefile

import (
	"errors"
	"io/fs"
	"net/url"
	"os"
	"strings"
)

// Create creates the missing database file a DSN names, 0600, before the
// driver opens it. The driver would create it with the process umask, which
// leaves it readable by every local user (#2417, #2422). An existing file
// keeps its mode, and SQLite gives its journal and WAL files the database
// file's mode.
func Create(dsn string) error {
	path := Path(dsn)
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// The umask can only take bits away; Chmod sets 0600 whatever it is.
	if err := f.Chmod(0o600); err != nil {
		f.Close() //nolint:errcheck,gosec // the Chmod error is the answer
		return err
	}
	return f.Close()
}

// Path returns the file a DSN would make SQLite create, or "" when it creates
// none: an in-memory database, or a mode other than rwc.
func Path(dsn string) string {
	path, query, _ := strings.Cut(dsn, "?")
	if rest, ok := strings.CutPrefix(path, "file:"); ok {
		if after, ok := strings.CutPrefix(rest, "//"); ok {
			i := strings.IndexByte(after, '/')
			if i < 0 {
				return ""
			}
			rest = after[i:]
		}
		p, err := url.PathUnescape(rest)
		if err != nil {
			return ""
		}
		path = p
	}
	if path == "" || path == ":memory:" {
		return ""
	}
	if v, err := url.ParseQuery(query); err == nil {
		if mode := v.Get("mode"); mode != "" && mode != "rwc" {
			return ""
		}
		if v.Get("vfs") == "memdb" {
			return ""
		}
	}
	return path
}
