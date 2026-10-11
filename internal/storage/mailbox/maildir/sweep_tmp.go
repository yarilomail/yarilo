package maildir

import (
	"log/slog"
	"path/filepath"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// SweepTemps removes what a save never published, at most once an hour per
// folder (mailbox.TempSweeper).
func (u *userMailbox) SweepTemps(folder string) {
	dir := filepath.Join(u.folderPath(folder), "tmp")
	if !mailbox.SweepDue(dir) {
		return
	}
	var removed []string
	err := u.withMailboxLockSite(folder, lockSiteSweepTemps, func() error {
		var serr error
		removed, serr = mailbox.SweepStaleTemps(dir, "") // tmp/ holds nothing else
		return serr
	})
	if err != nil {
		slog.Warn("maildir: a stale temp could not be removed",
			"user", u.username, "folder", folder, "err", err)
	}
	for _, name := range removed {
		slog.Info("maildir: removed a save that never got a name",
			"user", u.username, "folder", folder, "file", name)
	}
}
