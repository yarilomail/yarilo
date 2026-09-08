package maildir

import (
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// sweepStaleTemps removes what a save never published. A young one is left: its
// caller is about to name it, and taking it would take a live message (#1736).
func (u *userMailbox) sweepStaleTemps(folder string) {
	dir := filepath.Join(u.folderPath(folder), "tmp")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, ierr := e.Info()
		if ierr != nil || time.Since(info.ModTime()) < mailbox.StaleTemp {
			continue
		}
		if rerr := os.Remove(filepath.Join(dir, e.Name())); rerr != nil && !os.IsNotExist(rerr) {
			slog.Warn("maildir: a stale temp could not be removed",
				"user", u.username, "folder", folder, "file", e.Name(), "err", rerr)
			continue
		}
		slog.Info("maildir: removed a save that never got a name",
			"user", u.username, "folder", folder, "file", e.Name())
	}
}
