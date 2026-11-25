package mailboxbase

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"testing"
)

// Only a file already gone is quiet; any other reason a record named no file
// stays a WARN.
func TestOnlyAFileAlreadyGoneIsQuiet(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want slog.Level
	}{
		{fmt.Errorf("maildir/by-uid: no file: %w", fs.ErrNotExist), slog.LevelDebug},
		{fmt.Errorf("maildir/by-uid: %w", fs.ErrPermission), slog.LevelWarn},
		{errors.New("the record carries no filename"), slog.LevelWarn},
		{nil, slog.LevelWarn},
	} {
		if got := namelessLevel(tc.err); got != tc.want {
			t.Errorf("namelessLevel(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
