package mailboxbase

import (
	"log/slog"
	"strings"
	"sync"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A maildir describes itself, and a store written before the record carried its
// own key keeps the key beside the index. Both are settled when the folder is
// opened, so every protocol sees the same mailbox: called from the IMAP SELECT
// alone, a POP3-only account saw neither (#1778, #1779).
type uidNameMigrator interface {
	MigrateUIDNames(mailbox.Box, *mailbox.Folder) (int, error)
}

type proactiveSyncer interface {
	ProactiveScan() bool
	SyncToken(folder string) string
	ReconcileIndex(box mailbox.Box, folder *mailbox.Folder) (mailbox.SyncStats, error)
}

// indexDirNamer is the index's own name for where a folder's files live, which
// is what the token cache is keyed by: one user's namespaces resolve to
// different directories, and their tokens must not be each other's.
type indexDirNamer interface {
	IndexDirFor(folder string) string
}

// syncTokenCache remembers the change token of the last successful reconcile,
// for the life of the process (#1248): a session-scoped cache is invalidated by
// the client's login pattern rather than by the data changing, so it misses the
// case it exists for -- a client that reconnects per cycle arrives warm at
// nothing and walks the directory every time.
//
// The token is its own proof: every open recomputes it and compares, so a
// cached value is a starting point, not an answer, and a stale one costs an
// extra reconcile. Deliberately no TTL.
type syncTokenCache struct {
	mu     sync.Mutex
	tokens map[string]string
	// maxEntries bounds the map. Overflow drops it whole rather than evicting
	// by age: the entries carry no age, and rebuilding one costs a reconcile
	// that would have happened anyway.
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

// tokenKey identifies one folder of one account's storage.
func (b *Box) tokenKey(folder string) string {
	dir := ""
	if n, ok := b.index.(indexDirNamer); ok {
		dir = n.IndexDirFor(folder)
	}
	return strings.Join([]string{b.store.Username(), dir, folder}, "\x00")
}

// settle runs the passes a session owes a folder it has just opened: the stored
// names move into the records, and a self-describing store is reconciled
// against the index. Reports whether either changed the record set.
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

// reconcile skips the walk while the store's token is the one the last
// successful pass saw. The token is cached only on success, so a scan or lock
// failure does not wedge the folder into a permanent skip.
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
		"user", b.store.Username(), "folder", folder,
		"imported", st.Imported, "expunged", st.Expunged, "updated", st.Updated,
		"relinked", st.Relinked)
	return true
}
