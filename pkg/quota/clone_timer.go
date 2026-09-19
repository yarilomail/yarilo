package quota

import (
	"context"
	"time"
)

// The mirror is advisory, so nothing on a session's path may wait for it. The
// reference sets a flag and arms a timeout (QUOTA_CLONE_FLUSH_DELAY_MSECS), and
// the write happens there; a save never waits for a dict (#1875).
//
// The state is per user, not per session: two sessions of one user share the
// timer and the last value, as the reference's is on mail_user.
type pending struct {
	usage Usage
	dirty bool
	armed bool
	refs  int // sessions holding this user open
}

// Mirror records the latest usage for user and arms the flush. It returns at
// once: the write happens on the timer's goroutine.
func (c *Clone) Mirror(user string, u Usage) {
	if c == nil {
		return
	}
	c.mu.Lock()
	p := c.pendingFor(user)
	p.usage, p.dirty = u, true
	arm := !p.armed
	if arm {
		p.armed = true
	}
	c.mu.Unlock()
	if arm {
		time.AfterFunc(c.delay, func() { c.flush(user) })
	}
}

// Acquire and Release count the sessions of one user. The last one to leave
// flushes what is still pending, which is where the reference flushes too
// (deinit_pre): a session that closes inside the delay must not lose its
// number, and an earlier session leaving must not write on behalf of the ones
// still open.
func (c *Clone) Acquire(user string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.pendingFor(user).refs++
	c.mu.Unlock()
}

func (c *Clone) Release(user string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	p, ok := c.state[user]
	if !ok {
		c.mu.Unlock()
		return
	}
	p.refs--
	last := p.refs <= 0
	if last {
		c.inflight.Add(1)
	}
	c.mu.Unlock()
	if last {
		// Off this goroutine as well: closing a session is not the place to
		// wait for a mirror either. Close() waits for these before returning.
		go func() {
			defer c.inflight.Done()
			c.flush(user)
		}()
	}
}

// Close flushes everything still pending and waits for the writes already on
// their way. A pod that stops otherwise loses every user whose timer had not
// fired and who had no session left to release them; the reference writes at
// deinit for the same reason.
func (c *Clone) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.closed, c.draining = true, true
	users := make([]string, 0, len(c.state))
	for user, p := range c.state {
		if p.dirty {
			users = append(users, user)
		}
	}
	c.mu.Unlock()
	for _, user := range users {
		c.flush(user)
	}
	c.mu.Lock()
	c.draining = false
	c.mu.Unlock()
	c.inflight.Wait()
}

// flush writes the last value held for user, if any, and disarms. Runs off the
// session's goroutine in every case.
func (c *Clone) flush(user string) {
	c.mu.Lock()
	if c.closed && !c.draining {
		// A timer that fires after Close has nothing to add: Close wrote what
		// was pending and is waiting for the writes already under way.
		c.mu.Unlock()
		return
	}
	p, ok := c.state[user]
	if !ok || !p.dirty {
		if ok {
			p.armed = false
			if p.refs <= 0 {
				delete(c.state, user)
			}
		}
		c.mu.Unlock()
		return
	}
	u := p.usage
	p.dirty, p.armed = false, false
	drop := p.refs <= 0
	if drop {
		delete(c.state, user)
	}
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	c.Write(ctx, user, u)
}

// pendingFor returns the state for user, creating it. c.mu must be held.
func (c *Clone) pendingFor(user string) *pending {
	if c.state == nil {
		c.state = map[string]*pending{}
	}
	p, ok := c.state[user]
	if !ok {
		p = &pending{}
		c.state[user] = p
	}
	return p
}

// writeTimeout bounds one mirror write; the session is not waiting for it, so
// it only has to be short enough that a wedged dict does not pile up writers.
const writeTimeout = 5 * time.Second
