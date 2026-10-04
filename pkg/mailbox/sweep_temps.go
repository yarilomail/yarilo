package mailbox

import (
	"os"
	"path/filepath"
	"time"
)

const (
	// SweepInterval is how often one directory is swept: the sweep cannot be
	// dropped, and every open is a directory read too many (#1801).
	SweepInterval = time.Hour
	// SweepStampName records when this directory was last swept, in the
	// directory it guards. Skipped by the sweep itself.
	SweepStampName = ".yarilo-swept"
)

// SweepDue reports whether this directory is due a sweep. One stat, taken
// before any hold: a folder swept an hour ago must cost nothing (#1801).
func SweepDue(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, SweepStampName))
	return err != nil || time.Since(fi.ModTime()) >= SweepInterval
}

// SweepStaleTemps removes bodies a save never published; a young one is left,
// its caller is about to name it (#1736). The caller holds and checked SweepDue.
func SweepStaleTemps(dir string) (removed []string, err error) {
	stamp := filepath.Join(dir, SweepStampName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for _, e := range entries {
		if e.Name() == SweepStampName {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || now.Sub(info.ModTime()) < StaleTemp {
			continue
		}
		if rerr := os.Remove(filepath.Join(dir, e.Name())); rerr != nil && !os.IsNotExist(rerr) {
			err = rerr
			continue
		}
		removed = append(removed, e.Name())
	}
	// Stamped after the pass, so a failure is retried rather than skipped for
	// an hour.
	if err == nil {
		if f, cerr := os.OpenFile(stamp, os.O_CREATE|os.O_WRONLY, 0o600); cerr == nil {
			f.Close() //nolint:errcheck
			_ = os.Chtimes(stamp, now, now)
		}
	}
	return removed, err
}
