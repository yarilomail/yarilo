package mailindex

import "sync"

// cachePathMu is the in-process tier of the cache pair's lock, keyed by path;
// the cross-process tier is the folder's MailboxKey. Entries are never removed.
var cachePathMu sync.Map // path -> *sync.RWMutex

// LockCachePath takes the in-process tier; shared lets two sessions of one pod
// read together, or sharing the cross-process key buys nothing (#1673).
func LockCachePath(path string, shared bool) func() {
	mu, _ := cachePathMu.LoadOrStore(path, &sync.RWMutex{})
	m, ok := mu.(*sync.RWMutex)
	if !ok {
		panic("mailindex: cache mutex map holds a foreign type")
	}
	if shared {
		m.RLock()
		return m.RUnlock
	}
	m.Lock()
	return m.Unlock
}
