//go:build unix

package filelock

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"golang.org/x/sys/unix"
)

// metricOverridden counts the dotlocks taken from a holder that was gone. A
// nonzero value names a crash, not a busy volume.
var metricOverridden = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "filelock_dotlock_overridden_total",
	Help: "Dotlocks taken over from a holder that no longer holds them, by how it was established: dead means the local pid in the lock file is gone, stale means nothing changed for the stale timeout.",
}, []string{"reason"}) // dead | stale

// DefaultStaleTimeout is the reference's for the same file: a dotlock whose
// file and whose protected file both sit unchanged this long is assumed
// abandoned (mail-transaction-log-private.h, 3*60).
const DefaultStaleTimeout = 3 * time.Minute

var (
	staleMu      sync.RWMutex
	staleTimeout = DefaultStaleTimeout
)

// SetStaleTimeout sets how long a dotlock may sit unchanged before a waiter
// takes it over. Zero disables overriding, as the reference's zero does.
func SetStaleTimeout(d time.Duration) {
	staleMu.Lock()
	staleTimeout = d
	staleMu.Unlock()
}

func staleAfter() time.Duration {
	staleMu.RLock()
	defer staleMu.RUnlock()
	return staleTimeout
}

// ownerLine is what a dotlock says about its holder. The host is what makes
// the pid answerable: a pid from another node means nothing here.
func ownerLine() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return fmt.Sprintf("%d %s\n", os.Getpid(), host)
}

// localPID reads the holder's pid, and only when the lock was minted on this
// host. Anything else answers "not ours to judge".
func localPID(lockPath string) (int, bool) {
	b, err := os.ReadFile(lockPath)
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 {
		return 0, false
	}
	host, herr := os.Hostname()
	if herr != nil || fields[1] != host {
		return 0, false
	}
	pid, perr := strconv.Atoi(fields[0])
	if perr != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// holderGone reports whether the pid in the lock file is a process that no
// longer exists on this host.
func holderGone(lockPath string) bool {
	pid, ok := localPID(lockPath)
	if !ok || pid == os.Getpid() {
		return false
	}
	err := unix.Kill(pid, 0)
	return errors.Is(err, unix.ESRCH)
}

// unchangedFor reports whether neither the lock nor the file it protects has
// changed within d. The protected file counts because a holder writing to it
// is working, however old its lock file is -- the reference checks both.
func unchangedFor(path, lockPath string, d time.Duration, now time.Time) (os.FileInfo, bool) {
	lst, err := os.Lstat(lockPath)
	if err != nil {
		return nil, false
	}
	if now.Sub(lst.ModTime()) < d {
		return lst, false
	}
	if pst, perr := os.Stat(path); perr == nil && now.Sub(pst.ModTime()) < d {
		return lst, false
	}
	return lst, true
}

// overrideDotlock removes a lock this waiter has decided is abandoned, but only
// while it is still the very file that was judged: between the decision and the
// removal the holder may have released it and someone else taken it.
func overrideDotlock(lockPath string, judged os.FileInfo, reason string) bool {
	now, err := os.Lstat(lockPath)
	if err != nil || !os.SameFile(judged, now) || !now.ModTime().Equal(judged.ModTime()) {
		return false
	}
	if rerr := os.Remove(lockPath); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		slog.Warn("filelock: the abandoned dotlock could not be removed", "lock", lockPath, "err", rerr)
		return false
	}
	metricOverridden.WithLabelValues(reason).Inc()
	slog.Warn("filelock: took over a dotlock nobody holds",
		"lock", lockPath, "reason", reason, "held_since", judged.ModTime().UTC().Format(time.RFC3339))
	return true
}

// touchWhileHeld keeps a live holder's lock from reading as abandoned: the
// stale rule is "nothing changed", so a long hold has to say that it is alive.
func touchWhileHeld(lockPath string, stale time.Duration) (stop func()) {
	if stale <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(stale / 3)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case now := <-t.C:
				_ = os.Chtimes(lockPath, now, now)
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// processLives reports whether a pid is a process on this host.
func processLives(pid int) bool {
	return !errors.Is(unix.Kill(pid, 0), unix.ESRCH)
}
