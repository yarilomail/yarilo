package filelock

import (
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// ErrNoSharedLock is a volume or a method that cannot give a shared lock: only
// flock binds a hold to its own open file, so a close elsewhere in the process
// drops nothing; fcntl drops every hold of the process, dotlock has no shared
// form.
var ErrNoSharedLock = errors.New("filelock: no shared lock here (flock is needed)")

// Mode is a hold's kind under TakeMode.
type Mode int

// Shared holds coexist; an Exclusive one admits nobody else.
const (
	Shared Mode = iota
	Exclusive
)

// ModeHold is one flock hold on its own descriptor; Waited says the first try
// found the file held.
type ModeHold struct {
	f      *os.File
	Waited bool
}

// TakeMode takes a flock hold of mode on path, waiting up to wait. Each hold
// opens its own descriptor, so two holds of one process arbitrate like two
// processes and no in-process lock is needed. Shared holds coexist and an
// exclusive one waits for them; flock queues nobody, so an exclusive take
// can lose to a stream of shared ones and comes back ErrBusy.
func TakeMode(path string, method Method, mode Mode, wait time.Duration) (*ModeHold, error) {
	if method != MethodFlock || volumeRefused(path) {
		return nil, ErrNoSharedLock
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("filelock: open %s: %w", path, err)
	}
	how := unix.LOCK_SH
	if mode == Exclusive {
		how = unix.LOCK_EX
	}
	deadline := time.Now().Add(wait)
	for tries := 0; ; tries++ {
		err := flockFn(int(f.Fd()), how|unix.LOCK_NB)
		switch {
		case err == nil:
			return &ModeHold{f: f, Waited: tries > 0}, nil
		case unsupported(err):
			_ = f.Close()
			reportFallback(path, method)
			return nil, ErrNoSharedLock
		case !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN):
			_ = f.Close()
			return nil, fmt.Errorf("filelock: lock %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("filelock: %s is held elsewhere after %s: %w", path, wait, ErrBusy)
		}
		time.Sleep(pollInterval)
	}
}

// flockFn is the seam a row uses to make the kernel refuse flock.
var flockFn = unix.Flock

// Release gives the hold back; closing the descriptor is what releases it.
func (h *ModeHold) Release() error {
	if h == nil || h.f == nil {
		return nil
	}
	err := h.f.Close()
	h.f = nil
	return err
}
