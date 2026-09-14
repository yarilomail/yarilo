package dboxv2

import (
	"log/slog"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// SweepTemps removes what a save never published, at most once an hour per
// folder: a rebuild never runs on a healthy account (#1801).
func (u *userMailbox) SweepTemps(folder string) {
	dir := u.folderPath(folder)
	if !mailbox.SweepDue(dir) {
		return
	}
	var removed []string
	err := u.withMailboxLockSite(folder, "sweep-temps", func() error {
		var serr error
		removed, serr = mailbox.SweepStaleTemps(dir)
		return serr
	})
	if err != nil {
		slog.Warn("sdbox: a stale temp could not be removed",
			"user", u.username, "folder", folder, "err", err)
	}
	for _, name := range removed {
		slog.Info("sdbox: removed a save that never got a name",
			"user", u.username, "folder", folder, "file", name)
	}
}
