package locks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"time"
)

// lostWakeBackstop covers a release announcement that never arrived -- a
// subscription dropped mid-reconnect. It is not how a handover happens.
const lostWakeBackstop = time.Second

// handleLockWait answers when the lock is the caller's, not when the resource
// happens to be free. The order is the backend's, shared by every replica (#1821).
func (s *Server) handleLockWait(ctx context.Context, w io.Writer, fields []string, peer string, shared bool) {
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

	deadline := time.Now().Add(limit)
	started := time.Now()
	backstop := time.NewTicker(lostWakeBackstop)
	defer backstop.Stop()

	for {
		mine, ferr := queue.AtFront(ctx, resource, ticket)
		if ferr != nil {
			s.logger.Error("locks: could not read the line", "resource", resource, "err", ferr)
			_ = writeFields(w, respError, "internal")
			return
		}
		if mine {
			id, current, aerr := s.tryAcquire(ctx, resource, owner, site, ttl, shared)
			switch {
			case aerr == nil:
				s.metrics.observeAcquire(time.Since(started).Seconds(), "ok")
				_ = writeFields(w, respOK, id)
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
		case <-wakes:
		case <-backstop.C:
			s.metrics.incWaitBackstop()
		}
	}
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
