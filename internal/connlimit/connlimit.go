// Package connlimit provides per-user@IP connection counting for IMAP/POP3/SMTP,
// enforcing the mail_max_userip_connections limit.
package connlimit

import "sync"

// Limiter tracks active connections per user@IP pair.
// A zero-value Limiter (or max ≤ 0) is unlimited.
type Limiter struct {
	mu    sync.Mutex
	count map[string]int
	max   int
}

// New creates a Limiter that allows at most max simultaneous connections
// per user@IP pair. max ≤ 0 means unlimited.
func New(max int) *Limiter {
	return &Limiter{count: make(map[string]int), max: max}
}

// Acquire increments the counter for (user, ip).
// Returns false (and does not increment) if the limit is already reached.
func (l *Limiter) Acquire(user, ip string) bool { return l.AcquireMax(user, ip, l.max) }

// AcquireMax is Acquire against max instead of the limiter's own: a user's
// own limit counts on the same counter as everyone's (#2460). max ≤ 0 always
// passes, and is still counted.
func (l *Limiter) AcquireMax(user, ip string, max int) bool {
	key := user + "\x00" + ip
	l.mu.Lock()
	defer l.mu.Unlock()
	if max > 0 && l.count[key] >= max {
		return false
	}
	if l.count == nil {
		l.count = make(map[string]int)
	}
	l.count[key]++
	return true
}

// Release decrements the counter for (user, ip).
// Safe to call even if Acquire was never called.
func (l *Limiter) Release(user, ip string) {
	key := user + "\x00" + ip
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.count[key] > 0 {
		l.count[key]--
		if l.count[key] == 0 {
			delete(l.count, key)
		}
	}
}

// Count returns the current connection count for (user, ip).
func (l *Limiter) Count(user, ip string) int {
	key := user + "\x00" + ip
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.count[key]
}
