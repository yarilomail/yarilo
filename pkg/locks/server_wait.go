package locks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// lostWakeBackstop covers a release announcement that never arrived -- a
// subscription dropped mid-reconnect. It is not how a handover happens.
const lostWakeBackstop = time.Second

// handleLockWait answers when the lock is the caller's, not when the resource
// happens to be free. The order is the backend's, shared by every replica (#1821).
// handleLockWait answers one waiting acquire. It reports whether the
// connection is still usable for another command: the waiting call no longer
// ends the connection, so a client pool can keep it (#1875).
func (s *Server) handleLockWait(ctx context.Context, conn net.Conn, fields []string, peer string, shared bool) bool {
	var w io.Writer = conn
	if len(fields) != 6 {
		_ = writeFields(w, respError, "bad_lock")
		return true
	}
	queue, ok := queueing(s.backend)
	if !ok {
		_ = writeFields(w, respError, "unknown_command")
		return true
	}
	resource, owner := fields[1], fields[2]
	site := SiteUnknown
	if fields[4] != "" {
		site = fields[4]
	}
	ttl, err := parseTTL(fields[3])
	if err != nil {
		_ = writeFields(w, respError, "bad_ttl")
		return true
	}
	limit, err := parseTTL(fields[5])
	if err != nil {
		_ = writeFields(w, respError, "bad_wait")
		return true
	}

	// The subscription comes before the place in line: a release between the
	// two would otherwise be announced to nobody and waited out in full.
	ticket := newTicket()
	wakes, unsubscribe, err := queue.Wakes(ctx, resource, ticket)
	if err != nil {
		s.logger.Error("locks: could not listen for releases", "peer", peer, "resource", resource, "err", err)
		_ = writeFields(w, respError, "internal")
		return true
	}
	defer unsubscribe()
	s.trace(ticket, resource, "subscribe", peer)

	ahead, err := queue.Enqueue(ctx, resource, ticket)
	if err != nil {
		s.logger.Error("locks: could not join the line", "peer", peer, "resource", resource, "err", err)
		_ = writeFields(w, respError, "internal")
		return true
	}
	defer func() {
		if derr := queue.Dequeue(context.WithoutCancel(ctx), resource, ticket); derr != nil {
			s.logger.Warn("locks: could not leave the line", "resource", resource, "err", derr)
		}
	}()
	s.trace(ticket, resource, "join", peer, "ahead", ahead)
	s.metrics.observeQueueDepth(ahead)

	// A caller that is gone must stop waiting: a grant it never receives is a
	// lock nobody releases, and the line behind it waits out the TTL (#1824).
	gone, stopWatch := watchClose(conn)
	// Every answer goes through reply, which stops the watcher first: the
	// order is the whole point (#1875).
	reply := func(fields ...string) error {
		stopWatch()
		return writeFields(w, fields...)
	}

	deadline := time.Now().Add(limit)
	started := time.Now()
	backstop := time.NewTicker(lostWakeBackstop)
	defer backstop.Stop()

	// The first contender may try at once; afterwards a turn is named, and one
	// that is not this ticket's costs nothing to ignore (#1824).
	mine := ahead == 0
	woken := false
	for {
		if mine {
			id, current, aerr := s.tryAcquire(ctx, resource, owner, site, ticket, ttl, shared)
			// The refusal names who holds it, so the trace says who takes the
			// lock out from under the head of the queue (#1809).
			s.trace(ticket, resource, "try", peer, "held_by", current.Owner, "held_site", current.Site)
			switch {
			case aerr == nil:
				s.trace(ticket, resource, "result", peer, "granted", true, "after_backstop", woken)
				s.metrics.observeAcquire(time.Since(started).Seconds(), "ok")
				// A grant nobody receives is a lock nobody releases, and it
				// stands until its TTL (#1824).
				if werr := reply(respOK, id); werr != nil {
					if rerr := s.backend.Release(context.WithoutCancel(ctx), id); rerr != nil {
						s.logger.Error("locks: the grant could not be delivered and the lock could not be released",
							"peer", peer, "resource", resource, "id", id, "write_err", werr, "err", rerr)
					} else {
						s.metrics.incUndeliveredGrant()
						s.logger.Warn("locks: the grant could not be delivered, so the lock was released",
							"peer", peer, "resource", resource, "id", id, "err", werr)
					}
					return false // the answer did not reach the peer
				}
				return true
			case errors.Is(aerr, ErrBusy):
				s.metrics.incBusy()
				if time.Now().After(deadline) {
					s.metrics.observeAcquire(time.Since(started).Seconds(), "busy")
					_ = reply(respBusy, current.Owner, current.Site)
					return true
				}
			default:
				s.metrics.observeAcquire(time.Since(started).Seconds(), "error")
				s.logger.Error("locks: acquire failed", "peer", peer, "resource", resource, "err", aerr)
				_ = reply(respError, "internal")
				return true
			}
		}
		select {
		case <-gone:
			s.metrics.incCallerGone()
			s.logger.Debug("locks: the caller left the line", "peer", peer, "resource", resource)
			return false // the peer is gone; there is no connection to keep
		case <-ctx.Done():
			_ = reply(respError, "cancelled")
			return true
		case <-s.closing:
			_ = reply(respError, "closing")
			return false
		case <-time.After(time.Until(deadline)):
			s.metrics.observeAcquire(time.Since(started).Seconds(), "busy")
			_ = reply(respBusy, "", SiteUnknown)
			return true
		case <-wakes:
			// The subscription only passes this ticket's own turn, so a signal
			// here is it (#1809).
			s.trace(ticket, resource, "received", peer)
			mine = true
		case <-backstop.C:
			// Asking the backend is the expensive half, so it is asked only
			// here -- and counted only when a turn was owed and never came.
			front, ferr := queue.AtFront(ctx, resource, ticket)
			if ferr != nil {
				s.logger.Error("locks: could not read the line", "resource", resource, "err", ferr)
				_ = reply(respError, "internal")
				return false
			}
			if front {
				s.metrics.incWaitBackstop()
				woken = true
				s.trace(ticket, resource, "backstop", peer)
			}
			mine = front
		}
	}
}

// watchClose closes the returned channel when the peer goes away: nothing is
// sent while a wait is outstanding, so any read ends it (#1824). That premise
// is what makes the watcher safe to run on a live connection, and it is also
// why it must be stopped before the answer is written -- see stop below.
//
// stop unblocks the watcher without closing the connection: a read deadline in
// the past ends the pending Read, and the deadline is cleared once the
// goroutine is out, so the connection goes back to the command loop clean. It
// is called before the answer, never after: a client that reads the answer may
// send its next command immediately, and a watcher still in Read would eat the
// first byte of it (#1875).
func watchClose(conn net.Conn) (gone <-chan struct{}, stop func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		var b [1]byte
		_, _ = conn.Read(b[:])
	}()
	var once sync.Once
	return done, func() {
		once.Do(func() {
			_ = conn.SetReadDeadline(time.Now().Add(-time.Second))
			<-done
			_ = conn.SetReadDeadline(time.Time{})
		})
	}
}

// trace names one acquisition at each step, so a slow one can be stitched
// together from the log by its ticket (#1809).
func (s *Server) trace(ticket, resource, step, peer string, kv ...any) {
	args := append([]any{"ticket", ticket, "resource", resource, "step", step, "peer", peer}, kv...)
	s.logger.Debug("locks: hand-off", args...)
}

func (s *Server) tryAcquire(ctx context.Context, resource, owner, site, ticket string, ttl time.Duration, shared bool) (string, Holder, error) {
	if shared {
		return s.backend.AcquireShared(ctx, resource, owner, site, ticket, ttl)
	}
	return s.backend.Acquire(ctx, resource, owner, site, ticket, ttl)
}

// newTicket names one place in line. Random rather than sequential: the order
// is the queue's, and a guessable ticket lets one caller take another's place.
func newTicket() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
