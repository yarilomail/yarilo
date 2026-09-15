//go:build unix

package filelock

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// pollInterval is how often a blocked taker retries. fcntl's blocking form
// would be one syscall, but it cannot be cancelled, and a writer that waits
// for ever on a wedged mount is worse than one that gives up (#1840).
const pollInterval = 2 * time.Millisecond

func lockFD(f *os.File, method Method, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := tryLock(f, method)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EACCES) {
			return fmt.Errorf("filelock: lock %s: %w", f.Name(), err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("filelock: %s is held elsewhere after %s: %w", f.Name(), wait, ErrBusy)
		}
		time.Sleep(pollInterval)
	}
}

func tryLock(f *os.File, method Method) error {
	if method == MethodFlock {
		return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	}
	lk := &unix.Flock_t{Type: unix.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	return unix.FcntlFlock(f.Fd(), unix.F_SETLK, lk)
}

func unlockFD(f *os.File, method Method) error {
	if method == MethodFlock {
		return unix.Flock(int(f.Fd()), unix.LOCK_UN)
	}
	lk := &unix.Flock_t{Type: unix.F_UNLCK, Whence: 0, Start: 0, Len: 0}
	return unix.FcntlFlock(f.Fd(), unix.F_SETLK, lk)
}

// ErrBusy is returned when the lock could not be taken within the wait.
var ErrBusy = errors.New("filelock: busy")

// takeDotlock creates <path>.lock exclusively: the mount's lock daemon is not
// in the path, only the atomicity of O_EXCL.
func takeDotlock(path string, wait time.Duration) (*Hold, error) {
	lockPath := path + ".lock"
	deadline := time.Now().Add(wait)
	for {
		f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return &Hold{f: f, method: MethodDotlock, path: lockPath}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("filelock: dotlock %s: %w", lockPath, err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("filelock: %s is held elsewhere after %s: %w", lockPath, wait, ErrBusy)
		}
		time.Sleep(pollInterval)
	}
}

func (h *Hold) releaseDotlock() error {
	err := h.f.Close()
	h.f = nil
	if rerr := os.Remove(h.path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return rerr
	}
	return err
}
