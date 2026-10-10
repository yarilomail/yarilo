package mailbox

import (
	"os"
	"path/filepath"
	"strings"
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

// SweepStaleTemps removes old unpublished saves named prefix*, never a message (#2172);
// young by mtime or ctime, one is about to be named (#1736, #2175). Caller checked SweepDue.
func SweepStaleTemps(dir, prefix string) (removed []string, err error) {
	stamp := filepath.Join(dir, SweepStampName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	now := sweepNow()
	for _, e := range entries {
		if e.Name() == SweepStampName || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || now.Sub(info.ModTime()) < StaleTemp {
			continue
		}
		// A save dates its temp to the INTERNALDATE just before publishing it,
		// which leaves the ctime fresh (#2175).
		if ct, ok := changeTime(info); ok && now.Sub(ct) < StaleTemp {
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

// sweepNow is the sweep's clock. Test seam: a test cannot age a ctime.
var sweepNow = time.Now

// SetSweepClock makes the sweep read now() and returns the restore. Tests only.
func SetSweepClock(now func() time.Time) func() {
	prev := sweepNow
	sweepNow = now
	return func() { sweepNow = prev }
}
