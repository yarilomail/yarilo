package acl

import (
	"context"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/pkg/dict"
)

// The owner registry answers "who granted me anything" -- a discovery
// question, not an access one: rights always come from the ACL files, and a
// stale answer costs visibility for a while, never access. So it is read on
// an interval rather than per command, as the reference does with its
// hour-long SHARED_NS_RETRY_SECS gate (#1875).
const OwnerScanTTL = time.Hour

// ownerCache holds one answer per registry path. The paths differ in who owns
// them: anyone/ and group/<g>/ are the same rows for everyone who reads them,
// so one process reads each once; user/<u>/ is that user's own.
type ownerCache struct {
	mu      sync.Mutex
	entries map[string]ownerEntry
	now     func() time.Time // test seam
}

type ownerEntry struct {
	owners []string
	read   time.Time
}

var owners = &ownerCache{entries: map[string]ownerEntry{}, now: time.Now}

// InvalidateOwner drops what this user was told, so their own SETACL shows in
// their next LIST instead of waiting out the interval. Another user's grant
// arrives with the interval, as in the reference.
func InvalidateOwner(user string) {
	owners.mu.Lock()
	defer owners.mu.Unlock()
	delete(owners.entries, fwdPrefix+"user/"+dict.Escape(user)+"/")
}

// InvalidateAll drops every cached answer. For the admin repair verb, which
// rewrites the registry under everyone.
func InvalidateAll() {
	owners.mu.Lock()
	defer owners.mu.Unlock()
	owners.entries = map[string]ownerEntry{}
}

// scans counts the registry paths actually read off the dict, so a row can
// assert "one scan, not one per LIST".
var scans = struct {
	mu sync.Mutex
	n  int
}{}

func countScan()  { scans.mu.Lock(); scans.n++; scans.mu.Unlock() }
func Scans() int  { scans.mu.Lock(); defer scans.mu.Unlock(); return scans.n }
func ResetScans() { scans.mu.Lock(); scans.n = 0; scans.mu.Unlock() }

// cachedPath returns the owners under one registry path, reading it only when
// nothing fresh is held. The read happens outside the mutex: a dict iteration
// is a round trip, and holding the map across it would queue every reader
// behind the first.
func (c *ownerCache) cachedPath(ctx context.Context, d dict.Dict, path string) ([]string, error) {
	c.mu.Lock()
	e, ok := c.entries[path]
	fresh := ok && c.now().Sub(e.read) < OwnerScanTTL
	c.mu.Unlock()
	if fresh {
		return e.owners, nil
	}

	countScan()
	list, err := scanPath(ctx, d, path)
	if err != nil {
		// A failed scan answers with what we last had rather than with
		// nothing: discovery degrades, it does not flip to empty.
		if ok {
			return e.owners, nil
		}
		return nil, err
	}
	c.mu.Lock()
	c.entries[path] = ownerEntry{owners: list, read: c.now()}
	c.mu.Unlock()
	return list, nil
}
