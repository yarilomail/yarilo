// Package counters keeps one account's change counter, which only rises, and
// its value at each data type's last change; a push stream resumes from it.
package counters

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// FileName is the counter file in the user's control root.
const FileName = "yarilo-counters"

// Type is a JMAP data type name.
type Type string

const (
	Email            Type = "Email"
	Mailbox          Type = "Mailbox"
	Thread           Type = "Thread"
	Quota            Type = "Quota"
	Identity         Type = "Identity"
	EmailSubmission  Type = "EmailSubmission"
	SieveScript      Type = "SieveScript"
	VacationResponse Type = "VacationResponse"
)

// Mail is what a written message moves: the message, the counts of its
// mailbox, its thread and the account's usage.
var Mail = []Type{Email, Mailbox, Thread, Quota}

// Snapshot is the counter as last written.
type Snapshot struct {
	Highest uint64
	ByType  map[Type]uint64
}

// ChangedAfter lists the types whose last change is later than since.
func (s Snapshot) ChangedAfter(since uint64) []Type {
	var out []Type
	for t, v := range s.ByType {
		if v > since {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Bumper is what a writer holds: the counter of the account it writes to.
type Bumper interface {
	Bump(types ...Type) error
}

// Discard is a Bumper for a writer that has no account behind it.
var Discard Bumper = discard{}

type discard struct{}

func (discard) Bump(...Type) error { return nil }

// Counter is one account's counter.
type Counter struct {
	path   string
	user   string
	owner  string
	locker locks.Locker
}

// For is the counter of the account ui describes.
func For(ui *mailbox.UserInfo, owner string, l locks.Locker) *Counter {
	return New(Dir(ui.Home, mailbox.ControlRoot(ui)), ui.Username, owner, l)
}

// Dir is where the counter lives: the home, which every writer is given, else
// the control root.
func Dir(home, controlRoot string) string {
	if home != "" {
		return home
	}
	return controlRoot
}

// New returns the counter in dir.
func New(dir, user, owner string, l locks.Locker) *Counter {
	return &Counter{path: filepath.Join(dir, FileName), user: user, owner: owner, locker: l}
}

// pathMu orders this process's writers per file; the locker orders processes.
var (
	pathMuMu sync.Mutex
	pathMu   = map[string]*sync.Mutex{}
)

func lockPath(path string) *sync.Mutex {
	pathMuMu.Lock()
	defer pathMuMu.Unlock()
	m := pathMu[path]
	if m == nil {
		m = &sync.Mutex{}
		pathMu[path] = m
	}
	return m
}

// Bump raises the counter and records the new value against each type.
func (c *Counter) Bump(types ...Type) error {
	if len(types) == 0 {
		return nil
	}
	mu := lockPath(c.path)
	mu.Lock()
	defer mu.Unlock()
	return c.withLock(func() error {
		s, err := Read(c.path)
		if err != nil {
			return err
		}
		s.Highest++
		for _, t := range types {
			s.ByType[t] = s.Highest
		}
		return write(c.path, s)
	})
}

// Read returns the counter at path without a lock: it is replaced whole, so a
// reader sees one version or the next. A missing file is zero.
func Read(path string) (Snapshot, error) {
	s := Snapshot{ByType: map[Type]uint64{}}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, fmt.Errorf("counters: read: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, val, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		v, perr := strconv.ParseUint(val, 10, 64)
		if perr != nil {
			continue
		}
		if name == "highest" {
			s.Highest = v
		} else {
			s.ByType[Type(name)] = v
		}
	}
	if err := sc.Err(); err != nil {
		return s, fmt.Errorf("counters: read: %w", err)
	}
	return s, nil
}

// Path is the counter file of the account whose control root is dir.
func Path(dir string) string { return filepath.Join(dir, FileName) }

func write(path string, s Snapshot) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("counters: mkdir: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "highest %d\n", s.Highest)
	names := make([]string, 0, len(s.ByType))
	for t := range s.ByType {
		names = append(names, string(t))
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "%s %d\n", n, s.ByType[Type(n)])
	}
	tmp, err := os.CreateTemp(dir, FileName+".tmp.*")
	if err != nil {
		return fmt.Errorf("counters: temp: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(b.String()); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("counters: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("counters: close: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("counters: rename: %w", err)
	}
	return nil
}

func (c *Counter) withLock(fn func() error) error {
	if c.locker == nil {
		return fn()
	}
	key := locks.CountersKey(c.user)
	if held, err := locks.Reentrant(c.locker, key, "counters-bump", false); err != nil {
		return err
	} else if held != locks.HoldNone {
		return fn()
	}
	ctx, cancel := context.WithTimeout(locks.WithSite(context.Background(), "counters-bump"), 35*time.Second)
	defer cancel()
	lk, err := locks.Acquire(ctx, c.locker, key, c.owner, 30*time.Second)
	if err != nil {
		return fmt.Errorf("counters: lock: %w", err)
	}
	defer func() { _ = c.locker.Unlock(ctx, lk.ID) }()
	return fn()
}
