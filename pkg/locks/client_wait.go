package locks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// errNoQueue is the server saying it does not queue; the caller polls instead.
var errNoQueue = errors.New("locks/client: server does not queue waiters")

// queueless remembers that answer per client, so the fallback is paid once and
// not on every acquisition against an older server.
type queueless struct{ known atomic.Bool }

// LockWaiting asks the server to answer when the lock is ours rather than when
// the resource happens to be free. It runs on a connection of its own: a
// request the server holds open would otherwise occupy one of the pooled slots
// that UNLOCK needs, and the release that ends the wait could not be sent.
func (c *Client) LockWaiting(ctx context.Context, resource, owner string, ttl, limit time.Duration, shared bool) (Lock, error) {
	if c.noQueue.known.Load() {
		return Lock{}, errNoQueue
	}
	owner = CheckOwner(owner)
	ttlStr, err := formatTTL(ttl)
	if err != nil {
		return Lock{}, err
	}
	waitStr, err := formatTTL(limit)
	if err != nil {
		return Lock{}, err
	}
	cmd := cmdLockWait
	if shared {
		cmd = cmdLockSharedWait
	}
	expires := time.Now().Add(ttl)

	resp, err := c.soloExchange(ctx, limit, cmd, resource, owner, ttlStr, CheckSite(ctx), waitStr)
	if err != nil {
		return Lock{}, err
	}
	if len(resp) == 0 {
		return Lock{}, fmt.Errorf("locks/client: empty lock response: %w", ErrProtocol)
	}
	switch resp[0] {
	case respOK:
		if len(resp) != 2 {
			return Lock{}, fmt.Errorf("locks/client: malformed OK response: %w", ErrProtocol)
		}
		c.recordHold(resource, resp[1], holdKind(shared))
		return Lock{ID: resp[1], Resource: resource, Owner: owner, ExpiresAt: expires}, nil
	case respBusy:
		current, site := "", SiteUnknown
		if len(resp) > 1 {
			current = resp[1]
		}
		if len(resp) > 2 && resp[2] != "" {
			site = resp[2]
		}
		return Lock{Resource: resource, Owner: current, Site: site}, ErrBusy
	case respError:
		if len(resp) > 1 && resp[1] == "unknown_command" {
			c.noQueue.known.Store(true)
			return Lock{}, errNoQueue
		}
		return Lock{}, fmt.Errorf("locks/client: server error: %s", strings.Join(resp[1:], " "))
	}
	return Lock{}, fmt.Errorf("locks/client: unexpected response %v: %w", resp, ErrProtocol)
}

func holdKind(shared bool) HoldMode {
	if shared {
		return HoldShared
	}
	return HoldExclusive
}

// soloExchange runs one command on a connection opened for it and closed after,
// with a deadline covering the wait the server is allowed to hold it open for.
func (c *Client) soloExchange(ctx context.Context, limit time.Duration, cmd ...string) ([]string, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("locks/client: connect: %w: %w", ErrUnavailable, err)
	}
	defer func() { _ = conn.Close() }()

	// The server may hold the request for the whole wait; the margin covers the
	// handshake and the reply after the grant.
	_ = conn.SetDeadline(time.Now().Add(limit + 5*time.Second))
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(time.Now().Add(limit+5*time.Second)) {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	if err := writeFields(conn, cmdVersion, protocolVersion); err != nil {
		return nil, fmt.Errorf("locks/client: handshake: %w", err)
	}
	rd := newReader(conn)
	hs, err := rd.readFields()
	if err != nil || len(hs) < 3 || hs[2] != respOK {
		return nil, fmt.Errorf("locks/client: handshake failed: %w", ErrProtocol)
	}
	if err := writeFields(conn, cmd...); err != nil {
		return nil, fmt.Errorf("locks/client: send: %w", err)
	}
	resp, err := rd.readFields()
	if err != nil {
		return nil, fmt.Errorf("locks/client: read: %w", err)
	}
	return resp, nil
}
