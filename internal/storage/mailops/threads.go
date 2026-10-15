package mailops

import (
	"context"
	"log/slog"
	"time"

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
	rec := func(context.Context) error {
		_, err := o.cfg.Threads.Record(user, path, mailbox.FormatObjectID(guid), raw)
		return err
	}
	ctx, cancel := context.WithTimeout(locks.WithSite(context.Background(), o.cfg.Protocol+"-threads"), threadLockTimeout)
	defer cancel()
	var err error
	if o.cfg.Locker != nil {
		err = locks.WithLock(ctx, o.cfg.Locker, locks.ThreadsKey(user),
			locks.Owner(user, o.user.LockID()), threadLockTTL, threadLockRenew, rec)
	} else {
		err = rec(ctx)
	}
	if err != nil {
		slog.Warn(o.cfg.Protocol+": message saved but not threaded", "user", user, "err", err)
	}
}
