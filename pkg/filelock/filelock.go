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
