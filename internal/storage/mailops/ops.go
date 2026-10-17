// Package mailops runs the mutations of one user's mail and what follows them:
// quota, the full-text index and mailbox events (#2207).
package mailops

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/internal/quotawarn"
	"github.com/yarilomail/yarilo/internal/storage/search"
	"github.com/yarilomail/yarilo/internal/userstate/threads"
	"github.com/yarilomail/yarilo/pkg/ftsproto"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/quota"
)

// Config is a process's wiring, shared by every user it serves.
type Config struct {
	// Protocol names the caller in logs.
	Protocol string
	Locker   locks.Locker
	FTS      ftsproto.Client
	// FTSAutoindex indexes on save; retraction runs whenever FTS is set.
	FTSAutoindex bool
	FTSMaxRecent int
	Quota        QuotaConfig
	// Threads records each saved message's conversation; nil leaves threading off.
	Threads *threads.Recorder
	// ThreadTimer, when set, observes each thread record, lock included.
	ThreadTimer func(time.Duration)
}

// QuotaConfig is the site-wide quota wiring.
type QuotaConfig struct {
	// Engine enforces limits on saves; off, usage is only reported.
	Engine bool
	Policy quota.Policy
	// MailSize refuses a single message larger than this, zero for none.
	MailSize int64
	Warner   *quotawarn.Runner
	Clone    *quota.Clone
}

// Ops is one user's session over one Box.
type Ops struct {
	cfg   Config
	box   mailbox.Box
	user  *mailbox.UserInfo
	Quota *Quota
}

// New opens a session for an interactive protocol; delivery uses NewDelivery.
func New(cfg Config, box mailbox.Box, user *mailbox.UserInfo) *Ops {
	return &Ops{cfg: cfg, box: box, user: user, Quota: newQuota(cfg.Quota, box, user, false)}
}

// NewDelivery opens a session for one delivery: storage grace applies, and the
// quota warnings fire at the check rather than after the commit.
func NewDelivery(cfg Config, box mailbox.Box, user *mailbox.UserInfo) *Ops {
	return &Ops{cfg: cfg, box: box, user: user, Quota: newQuota(cfg.Quota, box, user, true)}
}

// Committed runs what follows a committed change to one message: the index,
// the quota usage and the event. vsize 0 means the size is not known here.
func (o *Ops) Committed(f *mailbox.Folder, ev locks.EventType, uid, vsize uint32, guid [16]byte) {
	if ev == locks.EventDelivered || ev == locks.EventExpunged {
		o.ftsNotify(f, ev == locks.EventExpunged, uid, guid)
		d, n := int64(vsize), int64(1)
		if ev == locks.EventExpunged {
			d, n = -d, -n
		}
		o.Quota.committed(vsize > 0, d, n)
	}
	o.EmitMessage(f.Name, ev, uid)
}

// EmitMessage publishes a change to one folder for IDLE sessions on any pod;
// best effort, the state is already on disk.
func (o *Ops) EmitMessage(folder string, ev locks.EventType, uid uint32) {
	if o.cfg.Locker == nil || o.user == nil || o.user.Username == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	payload := strconv.FormatUint(uint64(uid), 10)
	if err := o.cfg.Locker.Emit(ctx, locks.MailboxKey(o.user.Username, folder), ev, payload); err != nil {
		slog.Debug(o.cfg.Protocol+": emit event failed", "folder", folder, "type", string(ev), "err", err)
	}
}

func (o *Ops) ftsNotify(f *mailbox.Folder, expunged bool, uid uint32, guid [16]byte) {
	client := o.cfg.FTS
	if client == nil || o.user == nil || f == nil || f.Name == "" || uid == 0 {
		return
	}
	if !expunged && !o.cfg.FTSAutoindex {
		return
	}
	user := o.user.Username
	// The folder GUID keys the index path; without one there is nothing to
	// index into (#1183). Its hex form is never empty, so test the bytes.
	if f.GUID == ([16]byte{}) {
		WarnFolderWithoutGUID(o.cfg.Protocol, user, f.Name)
		return
	}
	mbox := search.RefOf(f)
	maxRecent := o.cfg.FTSMaxRecent
	go func() {
		var err error
		if expunged {
			err = client.Expunge(user, mbox, uid, guid)
		} else {
			err = client.Index(user, mbox, uid, maxRecent)
		}
		if err != nil {
			slog.Warn(o.cfg.Protocol+": fts notify failed",
				"user", user, "folder", mbox.Name, "expunged", expunged, "uid", uid, "err", err)
			return
		}
		slog.Debug(o.cfg.Protocol+": fts notify sent",
			"user", user, "folder", mbox.Name, "uid", uid, "expunged", expunged)
	}()
}

// folderGUIDWarned keeps the missing-GUID warning to one per folder per
// process: it is a property of the folder, not of the message.
var folderGUIDWarned sync.Map

// WarnFolderWithoutGUID logs, once per folder, that it cannot be indexed.
func WarnFolderWithoutGUID(protocol, user, folder string) {
	if _, seen := folderGUIDWarned.LoadOrStore(user+"\x00"+folder, struct{}{}); seen {
		return
	}
	slog.Warn(protocol+": folder has no GUID; full-text indexing skipped for it",
		"user", user, "folder", folder)
}

// Retracted takes one expunged message out of the index, for a change that
// moves no quota and wakes nobody (a rebuild's expunged copies).
func (o *Ops) Retracted(f *mailbox.Folder, uid uint32, guid [16]byte) {
	o.ftsNotify(f, true, uid, guid)
}
