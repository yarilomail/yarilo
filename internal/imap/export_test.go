package imap

import (
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// SetTestSessionID stands in for the login proxy's preamble, so a test
// connection can carry a session id and the two owner spellings stay
// distinguishable (#1652).
func SetTestSessionID(id string) { testSessionID = id }

// SetEnvCacheObserver watches the options every FETCH opens the envelope cache
// with, so the sharing decision can be asserted without racing two clients.
func SetEnvCacheObserver(fn func(mailbox.EnvelopeCacheOptions)) func() {
	prev := openEnvCache
	openEnvCache = func(box mailbox.Box, folderID uint64, o mailbox.EnvelopeCacheOptions) mailbox.EnvelopeCache {
		fn(o)
		return prev(box, folderID, o)
	}
	return func() { openEnvCache = prev }
}
