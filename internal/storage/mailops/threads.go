package mailops

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/userstate/threads"
	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// The account's thread lock: a save would rather be slow than blocked, and a
// lock nobody releases must not hold an account.
const (
	threadLockTimeout = 10 * time.Second
	threadLockTTL     = 15 * time.Second
	threadLockRenew   = 5 * time.Second
	// mergeLockWait is how long a save waits to merge: another merge running
	// is the one that folds this log, so it does not wait for it.
	mergeLockWait = 100 * time.Millisecond
)

// recordThread puts a saved message into its conversation (#2217); a failure
// leaves the mail as it is, and the migration step rebuilds the sidecar.
func (o *Ops) recordThread(guid [16]byte, raw []byte) {
	if o.cfg.Threads == nil || o.user == nil || guid == ([16]byte{}) {
		return
	}
	path := threads.PathFor(o.user)
	if path == "" {
		return
	}
	if timer := o.cfg.ThreadTimer; timer != nil {
		defer func(start time.Time) { timer(time.Since(start)) }(time.Now())
	}
	user := o.user.Username
	owner := locks.Owner(user, o.user.LockID())
	wrote := false
	rec := func(context.Context) (err error) {
		_, wrote, err = o.cfg.Threads.RecordNew(user, path, mailbox.FormatObjectID(guid), raw)
		return err
	}
	ctx, cancel := context.WithTimeout(locks.WithSite(context.Background(), o.cfg.Protocol+"-threads"), threadLockTimeout)
	defer cancel()
	var err error
	if o.cfg.Locker != nil {
		err = locks.WithLock(ctx, o.cfg.Locker, locks.ThreadsKey(user),
			owner, threadLockTTL, threadLockRenew, rec)
	} else {
		err = rec(ctx)
	}
	if err != nil {
		metricThreadRecordFailed.Inc()
		slog.Warn(o.cfg.Protocol+": message saved but not threaded", "user", user, "err", err)
		return
	}
	// The sidecar is written after the folder's event went out: a reader
	// between the two saw the old conversation.
	if wrote {
		locks.EmitBest(o.cfg.Locker, locks.ThreadsKey(user), locks.EventThreads, "")
	}
	o.mergeThreads(user, path)
}

// merging is the process's one thread merge in flight; a due merge that
// finds it taken is left to the account's next save.
var merging atomic.Bool

// mergeSeam, when set, runs in the merge's goroutine before and after it.
var mergeSeam func(done bool)

// mergeThreads folds the log into the base once it passes the tail size, in
// the background: the save returns first (THREADS-STORE.md §4, #2651). The
// rewrite runs under the merge's own lock, and only the log's swap takes the
// account's; a process stopped mid-merge leaves a pair the next one finishes.
func (o *Ops) mergeThreads(user, path string) {
	if !o.cfg.Threads.MergeDue(path) || !merging.CompareAndSwap(false, true) {
		return
	}
	go func() {
		if mergeSeam != nil {
			mergeSeam(false)
		}
		// Cleared before the seam says so: a test that saves on "finished"
		// must find no merge running.
		defer func() {
			merging.Store(false)
			if mergeSeam != nil {
				mergeSeam(true)
			}
		}()
		o.mergeNow(user, path)
	}()
}

func (o *Ops) mergeNow(user, path string) {
	owner := locks.Owner(user, o.user.LockID())
	hold := func(fn func() error) error {
		if o.cfg.Locker == nil {
			return fn()
		}
		ctx, cancel := context.WithTimeout(locks.WithSite(context.Background(), o.cfg.Protocol+"-threads-swap"), threadLockTimeout)
		defer cancel()
		return locks.WithLock(ctx, o.cfg.Locker, locks.ThreadsKey(user), owner, threadLockTTL, threadLockRenew,
			func(context.Context) error { return fn() })
	}
	merge := func(context.Context) error { return threads.MergeStore(path, hold) }
	var err error
	if o.cfg.Locker == nil {
		err = merge(context.Background())
	} else {
		ctx, cancel := context.WithTimeout(locks.WithSite(context.Background(), o.cfg.Protocol+"-threads-merge"), mergeLockWait)
		defer cancel()
		err = locks.WithLock(ctx, o.cfg.Locker, locks.ThreadsMergeKey(user), owner, threadLockTTL, threadLockRenew, merge)
	}
	if err != nil {
		slog.Warn(o.cfg.Protocol+": thread log not merged into its base", "user", user, "err", err)
	}
}

// A reader threads what a save left out (#2420): it waits a second for the
// lock, not a delivery's ten, and records a bounded number per read, so a
// fetch over many gaps cannot hold the account's lock for long.
const (
	lateThreadWait = time.Second
	lateThreadMax  = 64
)

// ThreadLate records messages of folder that the sidecar does not have, as
// their save would have: one header read and one append each. It returns the
// conversations recorded, by GUID, and the lock's refusal; a message it could
// not record keeps its own id and the next read tries again. An account without
// a sidecar is the migration's to build (#1425), so it records nothing.
func (o *Ops) ThreadLate(folder string, msgs []*mailbox.MessageMeta) (map[string]string, error) {
	if o.cfg.Threads == nil || o.user == nil || o.box == nil || len(msgs) == 0 {
		return nil, nil
	}
	path := threads.PathFor(o.user)
	if path == "" {
		return nil, nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil, nil
	}
	if len(msgs) > lateThreadMax {
		msgs = msgs[:lateThreadMax]
	}
	user := o.user.Username
	// Headers are read before the lock, as a save captures its own: only the
	// append waits on the account's lock, never the disk.
	type late struct {
		guid string
		raw  []byte
	}
	todo := make([]late, 0, len(msgs))
	for _, m := range msgs {
		raw, err := o.readHeader(folder, m)
		if err != nil {
			slog.Debug(o.cfg.Protocol+": late thread: header unreadable", "user", user, "err", err)
			continue
		}
		todo = append(todo, late{mailbox.FormatObjectID(m.GUID), raw})
	}
	if len(todo) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(todo))
	wrote := false
	rec := func(context.Context) error {
		for _, l := range todo {
			guid, raw := l.guid, l.raw
			id, w, err := o.cfg.Threads.RecordNew(user, path, guid, raw)
			if err != nil {
				return err
			}
			out[guid] = id
			if w {
				wrote = true
				metricThreadLate.Inc()
			}
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(locks.WithSite(context.Background(), o.cfg.Protocol+"-threads-late"), lateThreadWait)
	defer cancel()
	var err error
	if o.cfg.Locker != nil {
		err = locks.WithLock(ctx, o.cfg.Locker, locks.ThreadsKey(user),
			locks.Owner(user, o.user.LockID()), threadLockTTL, threadLockRenew, rec)
	} else {
		err = rec(ctx)
	}
	if err != nil {
		slog.Debug(o.cfg.Protocol+": late thread: not recorded this time", "user", user, "err", err)
	}
	if wrote {
		locks.EmitBest(o.cfg.Locker, locks.ThreadsKey(user), locks.EventThreads, "")
		o.mergeThreads(user, path)
	}
	return out, err
}

// readHeader reads a stored message up to the end of its header block.
func (o *Ops) readHeader(folder string, m *mailbox.MessageMeta) ([]byte, error) {
	rc, err := o.box.OpenMessage(folder, m)
	if err != nil {
		return nil, err
	}
	defer rc.Close() //nolint:errcheck
	var h headerCapture
	buf := make([]byte, 32<<10)
	for !h.done {
		n, err := rc.Read(buf)
		h.Write(buf[:n]) //nolint:errcheck,gosec // headerCapture never fails
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return h.buf, nil
}

// maxThreadHeader bounds what a save keeps for threading: the conversation is
// read from the header block, and a body is never held for it.
const maxThreadHeader = 256 << 10

// headerCapture keeps the bytes written to it up to the end of the header
// block, the first empty line, or maxThreadHeader.
type headerCapture struct {
	buf  []byte
	done bool
}

func (h *headerCapture) Write(p []byte) (int, error) {
	if h.done {
		return len(p), nil
	}
	start := max(len(h.buf)-3, 0)
	h.buf = append(h.buf, p...)
	if i := bytes.Index(h.buf[start:], []byte("\r\n\r\n")); i >= 0 {
		h.buf, h.done = h.buf[:start+i+4], true
	} else if i := bytes.Index(h.buf[start:], []byte("\n\n")); i >= 0 {
		h.buf, h.done = h.buf[:start+i+2], true
	}
	if len(h.buf) > maxThreadHeader {
		h.buf, h.done = h.buf[:maxThreadHeader], true
	}
	return len(p), nil
}
