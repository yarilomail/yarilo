package mailops

import (
	"bytes"
	"context"
	"log/slog"
	"time"

	"github.com/yarilomail/yarilo/internal/userstate/counters"
	"github.com/yarilomail/yarilo/internal/userstate/threads"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The account's thread lock: a save would rather be slow than blocked, and a
// lock nobody releases must not hold an account.
const (
	threadLockTimeout = 10 * time.Second
	threadLockTTL     = 15 * time.Second
	threadLockRenew   = 5 * time.Second
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
		slog.Warn(o.cfg.Protocol+": message saved but not threaded", "user", user, "err", err)
		return
	}
	// The sidecar is written after the folder's commit moved Thread, so it
	// moves again: a reader between the two saw the old conversation.
	if wrote {
		if berr := counters.For(o.user).Bump(counters.Thread); berr != nil {
			slog.Warn(o.cfg.Protocol+": change counter not moved", "user", user, "err", berr)
		}
	}
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
