package mailboxbase

import (
	"log/slog"
	"strings"
	"sync"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Both passes run at the open, so every protocol settles the same mailbox:
// from the IMAP SELECT alone, a POP3-only account saw neither (#1778, #1779).
type uidNameMigrator interface {
	MigrateUIDNames(mailbox.Box, *mailbox.Folder) (int, error)
}

type proactiveSyncer interface {
	ProactiveScan() bool
	SyncToken(folder string) string
	ReconcileIndex(box mailbox.Box, folder *mailbox.Folder) (mailbox.SyncStats, error)
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
	tokens map[string]string
	// maxEntries bounds the map; overflow drops it whole, since the entries
	// carry no age and rebuilding one costs a reconcile.
	maxEntries int
}

func (c *syncTokenCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.tokens[key]
	return v, ok
}

func (c *syncTokenCache) put(key, token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokens == nil {
		c.tokens = make(map[string]string)
	}
	if len(c.tokens) >= c.maxEntries {
		slog.Info("mailbox/sync: the token cache reached its bound and was dropped",
			"entries", len(c.tokens), "max", c.maxEntries)
		c.tokens = make(map[string]string)
	}
	c.tokens[key] = token
}

// syncTokens is the process-wide instance. 100k folders of tokens is a few MB;
// the cap is a bound, not a tuning knob.
var syncTokens = &syncTokenCache{maxEntries: 100_000}

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
	changed := b.adoptNames(folder, f)
	if b.reconcile(folder, f) {
		changed = true
	}
	return changed
}

func (b *Box) adoptNames(folder string, f *mailbox.Folder) bool {
	m, ok := mailbox.Driver(b.store).(uidNameMigrator)
	if !ok {
		return false
	}
	n, err := m.MigrateUIDNames(b, f)
	if err != nil {
		slog.Warn("mailbox/open: the stored names did not move into the records",
			"user", b.store.Username(), "folder", folder, "err", err)
		return false
	}
	return n > 0
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
	if token != "" {
		if prev, seen := syncTokens.get(key); seen && prev == token {
			MetricReconcile.WithLabelValues("skipped").Inc()
			return false
		}
	}
	MetricReconcile.WithLabelValues("scanned").Inc()
	st, err := ps.ReconcileIndex(b, f)
	if err != nil {
		slog.Warn("mailbox/open: the reconcile did not finish",
			"user", b.store.Username(), "folder", folder, "err", err)
		return false
	}
	syncTokens.put(key, token)
	if !st.Changed {
		return false
	}
	slog.Info("mailbox/open: the folder took what the store holds",
		"user", b.store.Username(), "folder", folder, "index_dir", b.indexDir(folder),
		"imported", st.Imported, "expunged", st.Expunged, "updated", st.Updated,
		"relinked", st.Relinked)
	return true
}
