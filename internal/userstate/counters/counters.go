// Package counters keeps one account's change counter, which only rises, and
// its value at each data type's last change; a push stream resumes from it.
package counters

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/pkg/filelock"
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
	Bump(site string, types ...Type) error
}

// Discard is a Bumper for a writer that has no account behind it.
var Discard Bumper = discard{}

type discard struct{}

func (discard) Bump(string, ...Type) error { return nil }

// Counter is one account's counter.
type Counter struct {
	path string
	user string
}

// For is the counter of the account ui describes.
func For(ui *mailbox.UserInfo) *Counter {
	return New(Dir(ui.Home, mailbox.ControlRoot(ui)), ui.Username)
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
func New(dir, user string) *Counter {
	return &Counter{path: filepath.Join(dir, FileName), user: user}
}

// lockWait bounds the wait for another process's bump of the same file.
const lockWait = 30 * time.Second

// maxKept bounds the lock files a process keeps open; idle ones past it close.
var maxKept = 256

// fileState is one counter file's writers in this process: the mutex orders
// them, the kept file lock orders processes.
type fileState struct {
	mu    sync.Mutex
	refs  int
	lock  *filelock.Kept
	dirOK bool
}

var (
	statesMu sync.Mutex
	states   = map[string]*fileState{}
)

func acquire(path string) *fileState {
	statesMu.Lock()
	defer statesMu.Unlock()
	st := states[path]
	if st == nil {
		// Beside the counter, not on it: a rename replaces the counter's inode.
		st = &fileState{lock: filelock.Keep(path+".lock", filelock.MethodFcntl)}
		states[path] = st
	}
	st.refs++
	return st
}

func release(st *fileState) {
	statesMu.Lock()
	defer statesMu.Unlock()
	st.refs--
	if len(states) <= maxKept {
		return
	}
	// Only a path no bump holds, and under its own mutex: closing any
	// descriptor of the file drops this process's fcntl lock on it.
	for p, s := range states {
		if s.refs == 0 && s.mu.TryLock() {
			_ = s.lock.Close()
			s.mu.Unlock()
			delete(states, p)
		}
	}
}

// Bump raises the counter and records the new value against each type; site
// names the write path for counters_bump_total.
func (c *Counter) Bump(site string, types ...Type) error {
	if len(types) == 0 {
		return nil
	}
	st := acquire(c.path)
	defer release(st)
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.dirOK {
		if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
			return fmt.Errorf("counters: mkdir: %w", err)
		}
		st.dirOK = true
	}
	unlock, err := st.lock.Take(lockWait)
	if err != nil {
		return fmt.Errorf("counters: lock: %w", err)
	}
	defer unlock()
	s, err := Read(c.path)
	if err != nil {
		return err
	}
	s.Highest++
	for _, t := range types {
		s.ByType[t] = s.Highest
	}
	if err := write(c.path, s); err != nil {
		return err
	}
	metricBumps.WithLabelValues(site).Inc()
	return nil
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

// write replaces the counter through a temporary file of this process's own;
// the caller holds the file lock, so no other process writes it meanwhile.
func write(path string, s Snapshot) error {
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
	tmp := path + ".tmp." + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("counters: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("counters: rename: %w", err)
	}
	return nil
}
