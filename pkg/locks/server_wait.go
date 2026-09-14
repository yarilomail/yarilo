package locks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"time"
)

// lostWakeBackstop covers a release announcement that never arrived -- a
// subscription dropped mid-reconnect. It is not how a handover happens.
const lostWakeBackstop = time.Second

// handleLockWait answers when the lock is the caller's, not when the resource
// happens to be free. The order is the backend's, shared by every replica (#1821).
func (s *Server) handleLockWait(ctx context.Context, conn net.Conn, fields []string, peer string, shared bool) {
	var w io.Writer = conn
	if len(fields) != 6 {
		_ = writeFields(w, respError, "bad_lock")
		return
	}
	queue, ok := queueing(s.backend)
	if !ok {
		_ = writeFields(w, respError, "unknown_command")
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

	// The subscription comes before the place in line: a release between the
	// two would otherwise be announced to nobody and waited out in full.
	wakes, unsubscribe, err := queue.Wakes(ctx, resource)
	if err != nil {
		s.logger.Error("locks: could not listen for releases", "peer", peer, "resource", resource, "err", err)
		_ = writeFields(w, respError, "internal")
		return
	}
	defer unsubscribe()

	ticket := newTicket()
	ahead, err := queue.Enqueue(ctx, resource, ticket)
	if err != nil {
		s.logger.Error("locks: could not join the line", "peer", peer, "resource", resource, "err", err)
		_ = writeFields(w, respError, "internal")
		return
	}
	defer func() {
		if derr := queue.Dequeue(context.WithoutCancel(ctx), resource, ticket); derr != nil {
			s.logger.Warn("locks: could not leave the line", "resource", resource, "err", derr)
		}
	}()
	s.metrics.observeQueueDepth(ahead)

	// A caller that is gone must stop waiting: a grant it never receives is a
	// lock nobody releases, and the line behind it waits out the TTL (#1824).
	gone := watchClose(conn)

	deadline := time.Now().Add(limit)
	started := time.Now()
	backstop := time.NewTicker(lostWakeBackstop)
	defer backstop.Stop()

	// The first contender may try at once; afterwards a turn is named, and one
	// that is not this ticket's costs nothing to ignore (#1824).
	mine := ahead == 0
	for {
		if mine {
			id, current, aerr := s.tryAcquire(ctx, resource, owner, site, ttl, shared)
			switch {
			case aerr == nil:
				s.metrics.observeAcquire(time.Since(started).Seconds(), "ok")
				// A grant nobody receives is a lock nobody releases, and it
				// stands until its TTL (#1824).
				if werr := writeFields(w, respOK, id); werr != nil {
					if rerr := s.backend.Release(context.WithoutCancel(ctx), id); rerr != nil {
						s.logger.Error("locks: the grant could not be delivered and the lock could not be released",
							"peer", peer, "resource", resource, "id", id, "write_err", werr, "err", rerr)
					} else {
						s.metrics.incUndeliveredGrant()
						s.logger.Warn("locks: the grant could not be delivered, so the lock was released",
							"peer", peer, "resource", resource, "id", id, "err", werr)
					}
				}
				return
			case errors.Is(aerr, ErrBusy):
				s.metrics.incBusy()
				if time.Now().After(deadline) {
					s.metrics.observeAcquire(time.Since(started).Seconds(), "busy")
					_ = writeFields(w, respBusy, current.Owner, current.Site)
					return
				}
			default:
				s.metrics.observeAcquire(time.Since(started).Seconds(), "error")
				s.logger.Error("locks: acquire failed", "peer", peer, "resource", resource, "err", aerr)
				_ = writeFields(w, respError, "internal")
				return
			}
		}
		select {
		case <-gone:
			s.metrics.incCallerGone()
			s.logger.Debug("locks: the caller left the line", "peer", peer, "resource", resource)
			return
		case <-ctx.Done():
			_ = writeFields(w, respError, "cancelled")
			return
		case <-s.closing:
			_ = writeFields(w, respError, "closing")
			return
		case <-time.After(time.Until(deadline)):
			s.metrics.observeAcquire(time.Since(started).Seconds(), "busy")
			_ = writeFields(w, respBusy, "", SiteUnknown)
			return
		case turn := <-wakes:
			// An unnamed turn is "whoever is first", which only a backend
			// that cannot name one sends.
			mine = turn == ticket || turn == ""
		case <-backstop.C:
			// Asking the backend is the expensive half, so it is asked only
			// here -- and counted only when a turn was owed and never came.
			front, ferr := queue.AtFront(ctx, resource, ticket)
			if ferr != nil {
				s.logger.Error("locks: could not read the line", "resource", resource, "err", ferr)
				_ = writeFields(w, respError, "internal")
				return
			}
			if front {
				s.metrics.incWaitBackstop()
			}
			mine = front
		}
	}
}

// watchClose closes the returned channel when the peer goes away: nothing is
// sent while a wait is outstanding, so any read ends it (#1824).
func watchClose(conn net.Conn) <-chan struct{} {
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		var b [1]byte
		_, _ = conn.Read(b[:])
	}()
	return gone
}

func (s *Server) tryAcquire(ctx context.Context, resource, owner, site string, ttl time.Duration, shared bool) (string, Holder, error) {
	if shared {
		return s.backend.AcquireShared(ctx, resource, owner, site, ttl)
	}
	return s.backend.Acquire(ctx, resource, owner, site, ttl)
}

// newTicket names one place in line. Random rather than sequential: the order
// is the queue's, and a guessable ticket lets one caller take another's place.
func newTicket() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
