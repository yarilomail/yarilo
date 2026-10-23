// Package filelock takes the kernel's lock on a file: what arbitrates writers
// on one volume, between processes and across hosts (#1840).
package filelock

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// metricFallback counts the volumes that refused the configured method. A
// nonzero value is a deployment running on dotlock without asking for it.
var metricFallback = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "filelock_method_fallback_total",
	Help: "Volumes whose kernel refused the configured lock method, counted once per volume. Writes there are held by dotlock instead.",
}, []string{"method"})

// refused remembers the volumes that answered "not supported", by device: the
// refusal belongs to the mount, and every later hold on it is a dotlock (#1850).
var (
	refusedMu sync.Mutex
	refused   = map[uint64]bool{}
)

func volumeRefused(path string) bool {
	dev, ok := deviceOf(path)
	if !ok {
		return false
	}
	refusedMu.Lock()
	defer refusedMu.Unlock()
	return refused[dev]
}

func reportFallback(path string, method Method) {
	dev, ok := deviceOf(path)
	seen := false
	if ok {
		refusedMu.Lock()
		seen = refused[dev]
		refused[dev] = true
		refusedMu.Unlock()
	}
	if seen {
		return
	}
	metricFallback.WithLabelValues(string(method)).Inc()
	slog.Warn("filelock: the volume refuses the configured lock method; writes here are held by dotlock",
		"volume", filepath.Dir(path), "method", method,
		"remedy", "set storage_lock_method: dotlock for an NFS mount without lockd")
}

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
	// stopTouch ends the keep-alive a dotlock runs while it is held.
	stopTouch func()
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
	// A volume that has refused once keeps the method it was given: mixing
	// the two on one file holds path.lock against a sibling's flock (#1850).
	if volumeRefused(path) {
		method = MethodDotlock
	}
	h, err := takeShared(path, method, time.Until(deadline))
	if unsupported(err) {
		reportFallback(path, method)
		h, err = takeShared(path, MethodDotlock, time.Until(deadline))
	}
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

// Kept is a lock file left open between takes. Closing any descriptor of a
// file drops its fcntl locks, so one path has one Kept per process.
type Kept struct {
	path   string
	method Method
	mu     sync.Mutex
	f      *os.File
}

// Keep returns a lock on path that opens its file once.
func Keep(path string, method Method) *Kept { return &Kept{path: path, method: method} }

// Take locks path; the returned func unlocks it and leaves the file open.
func (k *Kept) Take(wait time.Duration) (func(), error) {
	deadline := time.Now().Add(wait)
	for !k.mu.TryLock() {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("filelock: %s is held by this process after %s: %w", k.path, wait, ErrBusy)
		}
		time.Sleep(pollInterval)
	}
	method := k.method
	if volumeRefused(k.path) {
		method = MethodDotlock
	}
	if method != MethodDotlock {
		err := k.lockOpen(method, time.Until(deadline))
		if err == nil {
			return func() { _ = unlockFD(k.f, method); k.mu.Unlock() }, nil
		}
		if !unsupported(err) {
			k.mu.Unlock()
			return nil, err
		}
		reportFallback(k.path, method)
		_ = k.closeLocked()
	}
	h, err := takeDotlock(k.path, time.Until(deadline))
	if err != nil {
		k.mu.Unlock()
		return nil, err
	}
	return func() { _ = h.releaseDotlock(); k.mu.Unlock() }, nil
}

func (k *Kept) lockOpen(method Method, wait time.Duration) error {
	if k.f == nil {
		f, err := os.OpenFile(k.path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return fmt.Errorf("filelock: open %s: %w", k.path, err)
		}
		k.f = f
	}
	return lockFD(k.f, method, wait)
}

// Close releases the file. The caller ensures no Take is in progress.
func (k *Kept) Close() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.closeLocked()
}

func (k *Kept) closeLocked() error {
	if k.f == nil {
		return nil
	}
	err := k.f.Close()
	k.f = nil
	return err
}
