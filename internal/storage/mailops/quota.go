package mailops

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/quota"
)

// cacheTTL bounds how long a display value is served before a recount.
const cacheTTL = time.Second

// overStatusMaxDelay bounds how long after login the userdb over-flag is
// trusted (the reference's 10s guard).
const overStatusMaxDelay = 10 * time.Second

// Refusal names why a save was refused; protocols word it their own way.
type Refusal int

const (
	RefusedMailSize Refusal = iota + 1
	RefusedMessageCount
	RefusedOverQuota
)

// RefusedError is a save the quota refused.
type RefusedError struct {
	Reason    Refusal
	Size, Max int64
}

func (e *RefusedError) Error() string {
	switch e.Reason {
	case RefusedMailSize:
		return fmt.Sprintf("Requested allocation size %d exceeds max mail size %d", e.Size, e.Max)
	case RefusedMessageCount:
		return "Too many messages in the mailbox"
	}
	return "Quota exceeded"
}

// Quota is one session's quota state: the short usage cache, the warning
// baseline and the once-per-session over-status check.
type Quota struct {
	cfg      QuotaConfig
	box      mailbox.Box
	user     *mailbox.UserInfo
	delivery bool

	cacheUsage quota.Usage
	cacheAt    time.Time
	snap       quota.Usage
	snapSet    bool

	overStatusChecked bool
	loginAt           time.Time
}

func newQuota(cfg QuotaConfig, box mailbox.Box, user *mailbox.UserInfo, delivery bool) *Quota {
	return &Quota{cfg: cfg, box: box, user: user, delivery: delivery, loginAt: time.Now()}
}

// Limits are the user's quota rules.
func (q *Quota) Limits() quota.Limits {
	if q.user == nil {
		return quota.Limits{}
	}
	return quota.ParseRules(q.user.QuotaRules)
}

// Changed drops the display cache after a change whose size is not known.
func (q *Quota) Changed() { q.cacheAt = time.Time{} }

// Usage sums the account's usage, naming the caller: counting locks every
// folder, so a total without one answers nothing (#1634).
func (q *Quota) Usage(reason string, useCache bool) (quota.Usage, error) {
	if q.box == nil {
		return quota.Usage{}, nil
	}
	if useCache && !q.cacheAt.IsZero() && time.Since(q.cacheAt) < cacheTTL {
		quota.MetricUsageCount.WithLabelValues("hit", reason).Inc()
		return q.cacheUsage, nil
	}
	quota.MetricUsageCount.WithLabelValues("miss", reason).Inc()
	entries, err := q.box.ListFolders()
	if err != nil {
		return quota.Usage{}, err
	}
	u := quota.CountUsage(q.box, mailbox.SelectableNames(entries), q.Limits())
	q.cacheUsage, q.cacheAt = u, time.Now()
	if os := q.cfg.Policy.OverStatus; os.Mask != "" && os.LazyCheck && !q.overStatusChecked {
		q.evalOverStatus(u)
	}
	return u, nil
}

// LoginOverStatus runs the quota_over_status check at login unless it is lazy.
func (q *Quota) LoginOverStatus() {
	if os := q.cfg.Policy.OverStatus; os.Mask != "" && !os.LazyCheck {
		if u, err := q.Usage("login-over-status", false); err == nil {
			q.evalOverStatus(u)
		}
	}
}

func (q *Quota) evalOverStatus(u quota.Usage) {
	os := q.cfg.Policy.OverStatus
	if os.Mask == "" || q.overStatusChecked || q.user == nil {
		return
	}
	if time.Since(q.loginAt) > overStatusMaxDelay {
		return
	}
	q.overStatusChecked = true
	flagged := q.user.QuotaOverFlag != "" && quota.WildcardMatchIcase(q.user.QuotaOverFlag, os.Mask)
	actual := quota.IsOverAny(u, q.cfg.Policy.Scale(q.Limits()))
	if actual != flagged {
		q.cfg.Warner.FireOverStatus(q.user.Username, q.user.Home, os.Execute, q.user.QuotaOverFlag, actual)
	}
}

// CheckSave refuses a save of bytes into folder that the quota does not allow;
// it runs before a UID is allocated. A failed usage read lets the save through.
func (q *Quota) CheckSave(folder string, bytes int64) error {
	if !q.cfg.Engine || q.user == nil {
		return nil
	}
	if ms := q.cfg.MailSize; ms > 0 && bytes > ms {
		return &RefusedError{Reason: RefusedMailSize, Size: bytes, Max: ms}
	}
	if mmc := q.cfg.Policy.MailboxMessageCount; mmc > 0 {
		if cur, ok := q.folderMessageCount(folder); ok && cur >= mmc {
			return &RefusedError{Reason: RefusedMessageCount}
		}
	}
	if q.delivery {
		return q.checkDelivery(folder, bytes)
	}
	lim, ignore := q.Limits().EffectiveLimits(folder)
	if ignore {
		return nil
	}
	effLim := q.cfg.Policy.Scale(lim)
	if effLim.Unlimited() {
		return nil
	}
	u, err := q.Usage("enforce", false)
	if err != nil {
		return nil
	}
	q.snap, q.snapSet = u, true
	if quota.IsOver(u, effLim, bytes, 1) {
		return &RefusedError{Reason: RefusedOverQuota}
	}
	return nil
}

// checkDelivery is the delivery limit: storage grace applies, and the
// warnings and the mirror take the accepted size here.
func (q *Quota) checkDelivery(folder string, bytes int64) error {
	if len(q.user.QuotaRules) == 0 {
		return nil
	}
	lim := q.Limits()
	effLim, ignore := lim.EffectiveLimits(folder)
	effLim = q.cfg.Policy.Scale(effLim)
	if ignore || effLim.Unlimited() {
		return nil
	}
	entries, _ := q.box.ListFolders()
	names := mailbox.SelectableNames(entries)
	// A folder no session has opened still sums its records (#1728).
	q.fillSizes(names)
	u := quota.CountUsage(q.box, names, lim)
	if quota.IsOverWithGrace(u, effLim, bytes, 1, q.cfg.Policy.StorageGrace) {
		return &RefusedError{Reason: RefusedOverQuota}
	}
	if len(q.cfg.Policy.Warnings) > 0 || q.cfg.Clone != nil {
		after := quota.Usage{StorageBytes: u.StorageBytes + bytes, Messages: u.Messages + 1}
		if len(q.cfg.Policy.Warnings) > 0 {
			q.cfg.Warner.Fire(q.user.Username, q.user.Home, q.cfg.Policy.Warnings, q.cfg.Policy.Scale(lim), u, after)
		}
		q.cfg.Clone.Mirror(q.user.Username, after)
	}
	return nil
}

func (q *Quota) fillSizes(folders []string) {
	for _, name := range folders {
		f, err := q.box.Folder(name, 0)
		if err != nil {
			continue
		}
		if _, ferr := q.box.FillSizeless(f); ferr != nil {
			slog.Warn("mailops: sizes not filled", "folder", name, "err", ferr)
		}
	}
}

func (q *Quota) folderMessageCount(folder string) (int64, bool) {
	box := q.box
	if !q.delivery {
		box = mailbox.Counting(box)
	}
	f, err := box.Folder(folder, 0)
	if err != nil {
		return 0, false
	}
	md, err := q.box.Metadata(f.ID)
	if err != nil {
		return 0, false
	}
	return int64(md.Messages), true
}

// CaptureSnap takes the current usage as the "before" side of a warning
// crossing; cached is enough, it is not a decision (#1634).
func (q *Quota) CaptureSnap() {
	if len(q.cfg.Policy.Warnings) == 0 {
		return
	}
	if u, err := q.Usage("warning-baseline", true); err == nil {
		q.snap, q.snapSet = u, true
	}
}

// SeedSnap captures a baseline only when none exists yet.
func (q *Quota) SeedSnap() {
	if !q.snapSet {
		q.CaptureSnap()
	}
}

// committed moves the usage by one committed change and fires the warnings
// and the mirror; a delivery did both at the check.
func (q *Quota) committed(sized bool, dBytes, dMessages int64) {
	if q.delivery {
		return
	}
	after, have := quota.Usage{}, false
	if sized {
		after, have = q.afterDelta(dBytes, dMessages)
	}
	if !have {
		q.Changed()
	}
	wantWarn := len(q.cfg.Policy.Warnings) > 0 && q.snapSet
	wantClone := q.cfg.Clone != nil
	if !wantWarn && !wantClone {
		return
	}
	if !have {
		var err error
		if after, err = q.Usage("post-write", true); err != nil {
			return
		}
	}
	if wantWarn {
		q.fireWarnings(after)
	}
	if wantClone && q.user != nil {
		q.cfg.Clone.Mirror(q.user.Username, after)
	}
}

// afterDelta builds on the cached total when it is fresh, so a session that
// knows what it changed does not sweep every folder (#1548). The timestamp is
// not refreshed: another session's change must still arrive within the TTL.
func (q *Quota) afterDelta(dBytes, dMessages int64) (quota.Usage, bool) {
	if q.cacheAt.IsZero() || time.Since(q.cacheAt) >= cacheTTL {
		return quota.Usage{}, false
	}
	u := q.cacheUsage
	u.StorageBytes = max(u.StorageBytes+dBytes, 0)
	u.Messages = max(u.Messages+dMessages, 0)
	q.cacheUsage = u
	return u, true
}

func (q *Quota) fireWarnings(after quota.Usage) {
	if len(q.cfg.Policy.Warnings) == 0 || !q.snapSet || q.user == nil {
		return
	}
	before := q.snap
	q.snap = after
	q.cfg.Warner.Fire(q.user.Username, q.user.Home, q.cfg.Policy.Warnings, q.cfg.Policy.Scale(q.Limits()), before, after)
}
