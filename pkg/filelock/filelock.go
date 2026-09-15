// Package filelock takes the kernel's lock on a file: what arbitrates writers
// on one volume, between processes and across hosts (#1840).
package filelock

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Method names the transport. flock is the default: a POSIX record lock dies
// on any close of that file in the process, and readers close it (#1840).
type Method string

const (
	MethodFcntl   Method = "fcntl"
	MethodFlock   Method = "flock"
	MethodDotlock Method = "dotlock"
)

// Parse reads the configured method, defaulting to flock.
func Parse(s string) (Method, error) {
	switch Method(s) {
	case "", MethodFlock:
		return MethodFlock, nil
	case MethodFcntl:
		return MethodFcntl, nil
	case MethodDotlock:
		return MethodDotlock, nil
	}
	return "", fmt.Errorf("filelock: unknown lock_method %q (want fcntl, flock or dotlock)", s)
}

// Hold is one taken lock. Release is idempotent.
type Hold struct {
	f      *os.File
	method Method
	path   string
	local  *sync.Mutex
}

// local serialises this process's own writers: the kernel arbitrates between
// processes, and nothing arbitrates between our goroutines (#1840).
var (
	localMu sync.Mutex
	locals  = map[string]*sync.Mutex{}
)

func localFor(path string) *sync.Mutex {
	localMu.Lock()
	defer localMu.Unlock()
	m, ok := locals[path]
	if !ok {
		m = &sync.Mutex{}
		locals[path] = m
	}
	return m
}

// Take blocks until the lock on path is ours, or wait elapses. The file is
// created if absent: a writer that finds none still needs somewhere to wait.
func Take(path string, method Method, wait time.Duration) (*Hold, error) {
	deadline := time.Now().Add(wait)
	local := localFor(path)
	// With a deadline on both levels: a mutex that does not know about the
	// wait turns a bounded wait into an unbounded one.
	for !local.TryLock() {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("filelock: %s is held by this process after %s: %w", path, wait, ErrBusy)
		}
		time.Sleep(pollInterval)
	}
	h, err := takeShared(path, method, time.Until(deadline))
	if err != nil {
		local.Unlock()
		return nil, err
	}
	h.local = local
	return h, nil
}

func takeShared(path string, method Method, wait time.Duration) (*Hold, error) {
	if method == MethodDotlock {
		return takeDotlock(path, wait)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("filelock: open %s: %w", path, err)
	}
	if err := lockFD(f, method, wait); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Hold{f: f, method: method, path: path}, nil
}

// Release gives the lock back.
func (h *Hold) Release() error {
	if h == nil || h.f == nil {
		return nil
	}
	if h.local != nil {
		defer func() { h.local.Unlock(); h.local = nil }()
	}
	if h.method == MethodDotlock {
		return h.releaseDotlock()
	}
	err := unlockFD(h.f, h.method)
	cerr := h.f.Close()
	h.f = nil
	if err != nil {
		return err
	}
	return cerr
}

// Verify asks the volume at startup whether it locks at all: a mount with no
// lock daemon answers ENOLCK, and learning that while serving mail means
// learning it as loss. Exclusion is proven only where one process can prove
// it: a POSIX record lock belongs to the process (#1840).
func Verify(dir string, method Method) error {
	probe := filepath.Join(dir, ".yarilo-lock-probe")
	defer func() { _ = os.Remove(probe) }()

	first, err := takeShared(probe, method, 2*time.Second)
	if err != nil {
		return fmt.Errorf("filelock/verify: %s cannot take a %s lock in %s: %w", probe, method, dir, err)
	}
	defer func() { _ = first.Release() }()

	if method == MethodFcntl {
		return nil
	}
	second, serr := takeShared(probe, method, 200*time.Millisecond)
	if serr == nil {
		_ = second.Release()
		return fmt.Errorf("filelock/verify: %s admitted two writers at once in %s: the volume does not arbitrate this method", method, dir)
	}
	return nil
}
