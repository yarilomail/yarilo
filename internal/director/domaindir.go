package director

import (
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DomainEntry records which backend serves a domain, under the same total
// order user assignments use: higher seq wins, a tie goes to the lower id.
type DomainEntry struct {
	Domain    string
	Host      string // "ip:port"
	ExpiresAt time.Time
	AssignSeq uint64
	AssignBy  string
}

func (e *DomainEntry) newer(o *DomainEntry) bool {
	if e.AssignSeq != o.AssignSeq {
		return e.AssignSeq > o.AssignSeq
	}
	return e.AssignBy < o.AssignBy
}

// DomainDir is the domain→backend directory: the same shape as UserDir with a
// different key, because a shared mailbox is only reachable where every member
// of its domain is (#1931, #1943).
//
// It carries one thing UserDir does not: the load a placement credits to a
// backend the moment it is made. The reference raises its per-host count inside
// user_directory_add (user-directory.c:160), not when a connection arrives --
// without that, a burst of first logins reads the same zeroes and lands on one
// backend, which is the defect #1931 measured in least_sessions.
type DomainDir struct {
	mu      sync.RWMutex
	byName  map[string]*DomainEntry
	pending map[string]int // backend ip -> placements not yet seen as sessions
	expire  time.Duration
	self    string
	clock   atomic.Uint64
}

// NewDomainDir creates the directory with the per-entry TTL and this
// director's id, stamped on local placements.
func NewDomainDir(expire time.Duration, self string) *DomainDir {
	return &DomainDir{
		byName:  make(map[string]*DomainEntry),
		pending: make(map[string]int),
		expire:  expire,
		self:    self,
	}
}

// DomainOf is the key: everything after the last "@", lowercased. A username
// with no domain answers "", which is never placed by domain.
func DomainOf(username string) string {
	at := strings.LastIndex(username, "@")
	if at < 0 || at == len(username)-1 {
		return ""
	}
	return strings.ToLower(username[at+1:])
}

func (d *DomainDir) tick() uint64 { return d.clock.Add(1) }

func (d *DomainDir) observe(seq uint64) {
	for {
		cur := d.clock.Load()
		if seq < cur || d.clock.CompareAndSwap(cur, seq) {
			return
		}
	}
}

// Get returns the live entry for a domain, or nil when there is none or it has
// expired. An expired entry is dropped and its credit released.
func (d *DomainDir) Get(domain string) *DomainEntry {
	domain = strings.ToLower(domain)
	d.mu.Lock()
	defer d.mu.Unlock()
	e := d.byName[domain]
	if e == nil {
		return nil
	}
	if time.Now().After(e.ExpiresAt) {
		d.dropLocked(domain, e)
		return nil
	}
	return e
}

// Set records a local placement and credits its backend. Returns the stamp the
// caller propagates.
func (d *DomainDir) Set(domain, host string) (seq uint64, by string) {
	domain = strings.ToLower(domain)
	d.mu.Lock()
	defer d.mu.Unlock()
	if cur := d.byName[domain]; cur != nil {
		d.releaseLocked(cur.Host)
	}
	e := &DomainEntry{
		Domain:    domain,
		Host:      host,
		ExpiresAt: time.Now().Add(d.expire),
		AssignSeq: d.tick(),
		AssignBy:  d.self,
	}
	d.byName[domain] = e
	d.creditLocked(host)
	return e.AssignSeq, e.AssignBy
}

// Merge applies a placement that arrived from a peer, under the same order.
// Returns the previous host when the merge moved the domain, so the caller
// kicks the sessions it left behind.
func (d *DomainDir) Merge(domain, host string, seq uint64, by string) (movedFrom string) {
	d.observe(seq)
	domain = strings.ToLower(domain)
	incoming := &DomainEntry{
		Domain:    domain,
		Host:      host,
		ExpiresAt: time.Now().Add(d.expire),
		AssignSeq: seq,
		AssignBy:  by,
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	cur := d.byName[domain]
	if cur != nil && !incoming.newer(cur) {
		return ""
	}
	old := ""
	if cur != nil {
		d.releaseLocked(cur.Host)
		if cur.Host != host {
			old = cur.Host
		}
	}
	d.byName[domain] = incoming
	d.creditLocked(host)
	return old
}

// Touch extends a domain's life without changing where it sits: a domain with
// traffic is not forgotten, so affinity outlives the gap between two logins.
func (d *DomainDir) Touch(domain string) bool {
	domain = strings.ToLower(domain)
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.byName[domain]
	if !ok {
		return false
	}
	e.ExpiresAt = time.Now().Add(d.expire)
	return true
}

// Delete forgets a domain and releases its credit.
func (d *DomainDir) Delete(domain string) {
	domain = strings.ToLower(domain)
	d.mu.Lock()
	defer d.mu.Unlock()
	if e := d.byName[domain]; e != nil {
		d.dropLocked(domain, e)
	}
}

// Seen releases the credit a placement took: the domain's traffic is now in
// the session counts, so counting it twice would send the next domain away
// from a backend that is not in fact busier.
func (d *DomainDir) Seen(domain string) {
	domain = strings.ToLower(domain)
	d.mu.Lock()
	defer d.mu.Unlock()
	if e := d.byName[domain]; e != nil {
		d.releaseLocked(e.Host)
	}
}

// Pending is the load credited to a backend by placements whose sessions have
// not arrived yet, keyed by IP.
func (d *DomainDir) Pending(ip string) int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.pending[ip]
}

// Domains lists the live domains on one backend IP.
func (d *DomainDir) Domains(ip string) []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var out []string
	now := time.Now()
	for name, e := range d.byName {
		if now.After(e.ExpiresAt) {
			continue
		}
		if hostIP(e.Host) == ip {
			out = append(out, name)
		}
	}
	return out
}

func (d *DomainDir) creditLocked(host string) { d.pending[hostIP(host)]++ }

func (d *DomainDir) releaseLocked(host string) {
	ip := hostIP(host)
	if d.pending[ip] > 0 {
		d.pending[ip]--
	}
}

func (d *DomainDir) dropLocked(domain string, e *DomainEntry) {
	d.releaseLocked(e.Host)
	delete(d.byName, domain)
}

// hostIP is the address half of "ip:port"; the session counts are keyed by it.
func hostIP(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}
