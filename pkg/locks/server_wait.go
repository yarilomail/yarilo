package locks

import (
	"context"
	"errors"
	"io"
	"time"
)

// crossNodeProbe bounds how long a queued contender waits for a release that
// happened on another server, which no local UNLOCK wakes it for.
const crossNodeProbe = 100 * time.Millisecond

// handleLockWait answers when the lock is the caller's, not when the resource
// happens to be free: the caller is queued, and only the front of the queue
// retries, so short holders cannot overtake an exclusive contender (#1821).
func (s *Server) handleLockWait(ctx context.Context, w io.Writer, fields []string, peer string, shared bool) {
	if len(fields) != 6 {
		_ = writeFields(w, respError, "bad_lock")
		return
	}
	resource, owner := fields[1], fields[2]
	site := SiteUnknown
	if fields[4] != "" {
		site = fields[4]
	}
	ttl, err := parseTTL(fields[3])
	if err != nil {
		_ = writeFields(w, respError, "bad_ttl")
		return
	}
	limit, err := parseTTL(fields[5])
	if err != nil {
		_ = writeFields(w, respError, "bad_wait")
		return
	}

	deadline := time.Now().Add(limit)
	entry, depth := s.waits.join(resource)
	defer s.waits.leave(resource, entry)
	s.metrics.observeQueueDepth(depth)

	start := time.Now()
	probe := time.NewTimer(0)
	if !probe.Stop() {
		<-probe.C
	}
	defer probe.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = writeFields(w, respError, "cancelled")
			return
		case <-s.closing:
			_ = writeFields(w, respError, "closing")
			return
		case <-entry.ready:
		case <-probe.C:
		}

		id, current, aerr := s.tryAcquire(ctx, resource, owner, site, ttl, shared)
		switch {
		case aerr == nil:
			s.metrics.observeAcquire(time.Since(start).Seconds(), "ok")
			s.rememberGrant(id, resource)
			_ = writeFields(w, respOK, id)
			return
		case errors.Is(aerr, ErrBusy):
			s.metrics.incBusy()
			if remaining := time.Until(deadline); remaining <= 0 {
				s.metrics.observeAcquire(time.Since(start).Seconds(), "busy")
				_ = writeFields(w, respBusy, current.Owner, current.Site)
				return
			} else if remaining < crossNodeProbe {
				probe.Reset(remaining)
			} else {
				probe.Reset(crossNodeProbe)
			}
		default:
			s.metrics.observeAcquire(time.Since(start).Seconds(), "error")
			s.logger.Error("locks: acquire failed", "peer", peer, "resource", resource, "err", aerr)
			_ = writeFields(w, respError, "internal")
			return
		}
	}
}

func (s *Server) tryAcquire(ctx context.Context, resource, owner, site string, ttl time.Duration, shared bool) (string, Holder, error) {
	if shared {
		return s.backend.AcquireShared(ctx, resource, owner, site, ttl)
	}
	return s.backend.Acquire(ctx, resource, owner, site, ttl)
}

func (s *Server) rememberGrant(id, resource string) {
	s.grantMu.Lock()
	defer s.grantMu.Unlock()
	s.granted[id] = resource
}

// forgetGrant returns the resource a lock was taken on, and empty for one this
// server did not grant -- another replica's, or one that expired.
func (s *Server) forgetGrant(id string) string {
	s.grantMu.Lock()
	defer s.grantMu.Unlock()
	resource := s.granted[id]
	delete(s.granted, id)
	return resource
}
