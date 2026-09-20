package mailboxbase

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Both passes run at the open, so every protocol settles the same mailbox:
// from the IMAP SELECT alone, a POP3-only account saw neither (#1778, #1779).
type proactiveSyncer interface {
	ProactiveScan() bool
	SyncToken(folder string) string
	ReconcileIndex(box mailbox.Box, idx mailbox.UserIndex, folder *mailbox.Folder) (mailbox.SyncStats, error)
}

// partialSyncer is a driver that can take the arrivals alone when the rest of
// the store has not moved. A partial pass never judges absence: a walk that did
// not read the store cannot say what is missing from it (#1875).
type partialSyncer interface {
	PartialScope(folder, prevToken string) bool
	ReconcileArrivals(box mailbox.Box, idx mailbox.UserIndex, folder *mailbox.Folder) (mailbox.SyncStats, error)
}

// syncWindower reports what the driver knows about its own timestamps: whether
// new mail may be sitting in a directory the mtime cannot yet vouch for, and
// how wide that window is. A driver without it is judged by its token alone.
type syncWindower interface {
	// SyncDirty reports a hot arrival directory, a changed-store directory
	// whose mtime is inside the window, and the window itself.
	SyncDirty(folder string) (arrivalHot, storeDirty bool, window time.Duration)
}

// indexDirNamer names where a folder's files live: one user's namespaces
// resolve to different directories, and their tokens are not each other's.
type indexDirNamer interface {
	IndexDirFor(folder string) string
}

// syncTokenCache holds the last reconcile's token for the life of the process:
// a session-scoped one is invalidated by the login pattern, not the data (#1248).
type syncTokenCache struct {
	mu     sync.Mutex
	tokens map[string]tokenSeen
	// maxEntries bounds the map; overflow drops it whole, since the entries
	// carry no age and rebuilding one costs a reconcile.
	maxEntries int
}

// tokenSeen is the last token and when the folder was last walked: the token
// alone cannot bound a dirty folder, whose token is new on every read.
type tokenSeen struct {
	token     string
	checkedAt time.Time
	// dirtyThen is whether the walk behind this entry was taken while the
	// store's mtime could not vouch for it. The reference compares the stored
	// check time against the stored mtime for the same reason: a change landing
	// in the same second as the check is invisible to both.
	dirtyThen bool
}

func (c *syncTokenCache) get(key string) (tokenSeen, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.tokens[key]
	return v, ok
}

func (c *syncTokenCache) put(key, token string, at time.Time, dirtyThen bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokens == nil {
		c.tokens = make(map[string]tokenSeen)
	}
	if len(c.tokens) >= c.maxEntries {
		slog.Info("mailbox/sync: the token cache reached its bound and was dropped",
			"entries", len(c.tokens), "max", c.maxEntries)
		c.tokens = make(map[string]tokenSeen)
	}
	c.tokens[key] = tokenSeen{token: token, checkedAt: at, dirtyThen: dirtyThen}
}

// syncTokens is the process-wide instance. 100k folders of tokens is a few MB;
// the cap is a bound, not a tuning knob.
var syncTokens = &syncTokenCache{maxEntries: 100_000}

// walkedFolder names each folder as it is walked. A counter cannot answer
// "which folder": a command that walks its source for a reason of its own
// hides whether it also walked its destination (#1875).
var walkedFolder func(string)

// afterWalk runs between the walk and the re-read of the token, which is the
// only window a row can land a change in.
var afterWalk func(string)

// SetAfterWalk registers that hook and returns a function removing it.
func SetAfterWalk(fn func(string)) func() {
	afterWalk = fn
	return func() { afterWalk = nil }
}

// SetWalkedFolder registers the hook and returns a function removing it.
func SetWalkedFolder(fn func(string)) func() {
	walkedFolder = fn
	return func() { walkedFolder = nil }
}

// SetTestSyncTokens swaps the cache and returns a function restoring it, so a
// row can start from a cold one.
func SetTestSyncTokens(c int) func() {
	prev := syncTokens
	syncTokens = &syncTokenCache{maxEntries: c}
	return func() { syncTokens = prev }
}

// indexDir is where this folder's index lives, which is what tells two accounts
// of the same name apart in a log (#1673).
func (b *Box) indexDir(folder string) string {
	if n, ok := b.index.(indexDirNamer); ok {
		return n.IndexDirFor(folder)
	}
	return ""
}

// tokenKey identifies one folder of one account's storage.
func (b *Box) tokenKey(folder string) string {
	dir := ""
	if n, ok := b.index.(indexDirNamer); ok {
		dir = n.IndexDirFor(folder)
	}
	return strings.Join([]string{b.store.Username(), dir, folder}, "\x00")
}

// settle runs both passes a session owes a folder it just opened, and reports
// whether either changed the record set.
func (b *Box) settle(folder string, f *mailbox.Folder) bool {
	b.sweepTemps(folder)
	return b.reconcile(folder, f)
}

// sweepTemps lets the driver clear what a save never published; the driver
// takes its own hold, and only when its interval gate says there is work.
func (b *Box) sweepTemps(folder string) {
	sw, ok := mailbox.Driver(b.store).(mailbox.TempSweeper)
	if !ok {
		return
	}
	sw.SweepTemps(folder)
}

// walk runs the pass the gate chose: the whole store, or the arrivals alone.
func (b *Box) walk(ps proactiveSyncer, folder string, f *mailbox.Folder, partial bool) (mailbox.SyncStats, error) {
	if partial {
		if pw, ok := ps.(partialSyncer); ok {
			return pw.ReconcileArrivals(b, b.index, f)
		}
	}
	return ps.ReconcileIndex(b, b.index, f)
}

// partialWalkable reports whether this walk may read the arrivals alone: the
// driver has to offer it, and the store half of the token must not have moved.
func (b *Box) partialWalkable(ps proactiveSyncer, folder, prevToken string) bool {
	pw, ok := ps.(partialSyncer)
	return ok && prevToken != "" && pw.PartialScope(folder, prevToken)
}

// syncDirtiness asks the driver what its own timestamps are worth right now. A
// driver that does not answer is judged by its token alone, as before.
func (b *Box) syncDirtiness(ps proactiveSyncer, folder string) (arrivalHot, storeDirty bool, window time.Duration) {
	sw, ok := ps.(syncWindower)
	if !ok {
		return false, false, 0
	}
	return sw.SyncDirty(folder)
}

// reconcile skips the walk while the token is the one the last successful pass
// saw; caching only on success keeps a failure from wedging a permanent skip.
func (b *Box) reconcile(folder string, f *mailbox.Folder) bool {
	ps, ok := mailbox.Driver(b.store).(proactiveSyncer)
	if !ok || !ps.ProactiveScan() {
		return false
	}
	key := b.tokenKey(folder)
	token := ps.SyncToken(folder)
	arrivalHot, _, window := b.syncDirtiness(ps, folder)
	// The cause is recorded, not only the decision: a folder this process has
	// never seen walks for a different reason than one whose mtime moved, and
	// a counter that cannot tell them apart cannot say what a restart costs
	// (#1875). The reference keeps the same four in enum maildir_scan_why.
	reason := reasonFirstSeen
	partial := false
	if token != "" {
		if prev, seen := syncTokens.get(key); seen {
			switch {
			case arrivalHot:
				reason = reasonHotNew
				// Only the arrivals, when the store has not moved: the walk
				// this case pays for is the one cur/ does not need.
				partial = b.partialWalkable(ps, folder, prev.token)
			case prev.token != token:
				// A moved mtime is always walked, as the reference walks on
				// DIR_MTIME_CHANGED; the window bounds re-walks of a dirty
				// directory that has not moved, nothing else (#1875).
				reason = reasonTokenMoved
			case prev.dirtyThen && time.Since(prev.checkedAt) < window:
				MetricReconcile.WithLabelValues("skipped-window", "").Inc()
				return false
			case prev.dirtyThen:
				// The window has passed: one walk is owed, because a change
				// landing in the same second as that walk moved neither the
				// mtime nor the token built from it.
				reason = reasonOwed
			default:
				MetricReconcile.WithLabelValues("skipped", "").Inc()
				return false
			}
		}
	}
	// Why it walks, not only how often: a folder whose driver gives no token
	// walks every open, and it counts apart from one whose token moved (#1821).
	switch {
	case token == "":
		MetricReconcile.WithLabelValues("scanned-untokened", reason).Inc()
	case partial:
		// Counted apart: the share of walks that read only the arrivals is
		// what says whether the partial pass is worth its definition.
		MetricReconcile.WithLabelValues("scanned-partial", reason).Inc()
	default:
		MetricReconcile.WithLabelValues("scanned", reason).Inc()
	}
	if walkedFolder != nil {
		walkedFolder(folder)
	}
	// The walk is what costs; the counter says how often, never how long.
	walked := time.Now()
	st, err := b.walk(ps, folder, f, partial)
	MetricReconcileSeconds.Observe(time.Since(walked).Seconds())
	if err != nil {
		slog.Warn("mailbox/open: the reconcile did not finish",
			"user", b.store.Username(), "folder", folder, "err", err)
		return false
	}
	if afterWalk != nil {
		afterWalk(folder)
	}
	// The directory as the walk left it, not as it found it: a walk that moves
	// new/ into cur/ changes cur's mtime itself, and storing the pre-walk token
	// makes the next open walk again for this process's own move. The reference
	// re-stats cur/ after the sync for the same reason
	// (maildir-sync-index.c:277-281). Nothing is lost by it: a change that
	// lands during the walk leaves the directory dirty, and the window's owed
	// re-walk takes it (#1941, #1875).
	after := ps.SyncToken(folder)
	if after == "" {
		after = token
	}
	hotAfter, dirtyAfter, _ := b.syncDirtiness(ps, folder)
	syncTokens.put(key, after, time.Now(), dirtyAfter || hotAfter)
	if !st.Changed {
		return false
	}
	slog.Info("mailbox/open: the folder took what the store holds",
		"user", b.store.Username(), "folder", folder, "index_dir", b.indexDir(folder),
		"imported", st.Imported, "expunged", st.Expunged, "updated", st.Updated,
		"relinked", st.Relinked)
	return true
}
