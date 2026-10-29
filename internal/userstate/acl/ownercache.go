package acl

import (
	"context"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/dict"
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
	// flight is the scan of a path in progress: a cold cache after a pod start
	// must not send every LISTing session to scan anyone/ at once (#2381).
	flight map[string]*ownerScan
	now    func() time.Time // test seam
}

// joinedScan lets a test see a miss wait for a scan already in flight.
var joinedScan = func() {}

// ownerScan is one registry read others wait for; done closes once it has an answer.
type ownerScan struct {
	done   chan struct{}
	owners []string
	err    error
}

type ownerEntry struct {
	owners []string
	read   time.Time
}

var owners = &ownerCache{entries: map[string]ownerEntry{}, flight: map[string]*ownerScan{}, now: time.Now}

// InvalidatePrincipals drops the answers a grant has just made wrong. The keys
// are read by the principal a grant names -- user/<seer>/, group/<g>/,
// anyone/ -- so it is the recipients' answers that go stale, not the owner's.
// On the backend that took the SETACL the recipient sees it at once; on every
// other backend it arrives with the interval, as in the reference.
func InvalidatePrincipals(paths []string) {
	if len(paths) == 0 {
		return
	}
	owners.mu.Lock()
	defer owners.mu.Unlock()
	for _, p := range paths {
		delete(owners.entries, fwdPrefix+p+"/")
	}
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
	if ok && c.now().Sub(e.read) < OwnerScanTTL {
		c.mu.Unlock()
		return e.owners, nil
	}
	if f := c.flight[path]; f != nil {
		c.mu.Unlock()
		joinedScan()
		select {
		case <-f.done:
			return f.owners, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &ownerScan{done: make(chan struct{})}
	c.flight[path] = f
	c.mu.Unlock()
	f.owners, f.err = c.scan(ctx, d, path, e, ok)
	c.mu.Lock()
	delete(c.flight, path)
	c.mu.Unlock()
	close(f.done)
	return f.owners, f.err
}

// scan reads one path and records the answer; e is the stale entry, if any.
func (c *ownerCache) scan(ctx context.Context, d dict.Dict, path string, e ownerEntry, ok bool) ([]string, error) {
	countScan()
	list, err := scanPath(ctx, d, path)
	if err != nil {
		// The attempt is what the interval counts, not its outcome: a dict
		// that is down would otherwise be asked again by every LIST. A failed
		// scan answers with what we last had -- discovery degrades, it does
		// not flip to empty.
		if ok {
			c.mu.Lock()
			c.entries[path] = ownerEntry{owners: e.owners, read: c.now()}
			c.mu.Unlock()
			return e.owners, nil
		}
		return nil, err
	}
	c.mu.Lock()
	c.entries[path] = ownerEntry{owners: list, read: c.now()}
	c.mu.Unlock()
	return list, nil
}
