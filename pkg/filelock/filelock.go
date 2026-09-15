// Package filelock takes the lock the kernel keeps on a file, which is what
// arbitrates writers on one volume — between threads, between the pod's
// protocol processes, and across hosts over NFS (#1840).
package filelock

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// Method names the transport. fcntl is the default and the one that works
// through NFS; dotlock is for a mount whose lock daemon is not trusted.
type Method string

const (
	MethodFcntl   Method = "fcntl"
	MethodFlock   Method = "flock"
	MethodDotlock Method = "dotlock"
)

// Parse reads the configured method, defaulting to fcntl.
func Parse(s string) (Method, error) {
	switch Method(s) {
	case "", MethodFcntl:
		return MethodFcntl, nil
	case MethodFlock:
		return MethodFlock, nil
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

// local serialises this process's own writers. A POSIX record lock belongs to
// the process, not to the descriptor, so the kernel arbitrates between
// processes and nothing arbitrates between our goroutines (#1840).
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

// Take blocks until the lock on path is this process's, or wait elapses. The
// file is created if it is not there: the lock is on the name, and a writer
// that finds no file still needs somewhere to wait.
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
