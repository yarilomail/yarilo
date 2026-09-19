package locks

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// LockWaiting is answered when the lock is ours. On a connection of its own: a
// held-open request in the pool would starve the UNLOCK that ends its wait.
func (c *Client) LockWaiting(ctx context.Context, resource, owner string, ttl, limit time.Duration, shared bool) (Lock, error) {
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

	resp, addr, err := c.soloExchange(ctx, limit, cmd, resource, owner, ttlStr, CheckSite(ctx), waitStr)
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
			// Loudly, not by polling: server and client ship in one image, so a
			// server that cannot queue is a rollout that did not finish (#1823).
			return Lock{}, fmt.Errorf("locks/client: server %s does not queue waiters: %w", addr, ErrProtocol)
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

// soloExchange runs one waiting command on a connection from the waiting pool,
// with a deadline covering the wait the server is allowed to hold it open for.
//
// A waiting call cannot share the control pool: the server may hold the request
// open for the whole wait, and a control slot held that long would starve every
// ordinary round trip. It does not need a new connection either -- opening one
// per call meant resolving the service name per call, which was 376 s of a
// 928 s waiting profile (#1875). So: its own pool, kept connected.
func (c *Client) soloExchange(ctx context.Context, limit time.Duration, cmd ...string) ([]string, string, error) {
	slot, err := c.takeWaitSlot(ctx)
	if err != nil {
		return nil, "", err
	}
	ok := false
	defer func() { c.putWaitSlot(slot, ok) }()

	if cerr := c.ensureConnectedIn(ctx, slot, "wait"); cerr != nil {
		return nil, "", fmt.Errorf("locks/client: connect: %w: %w", ErrUnavailable, cerr)
	}
	conn := slot.conn
	addr := conn.RemoteAddr().String()

	// The server may hold the request for the whole wait; the margin covers the
	// reply after the grant.
	_ = conn.SetDeadline(time.Now().Add(limit + 5*time.Second))
	if deadline, dok := ctx.Deadline(); dok && deadline.Before(time.Now().Add(limit+5*time.Second)) {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	if err := writeFields(conn, cmd...); err != nil {
		return nil, addr, fmt.Errorf("locks/client: send: %w", err)
	}
	resp, err := slot.reader.readFields()
	if err != nil {
		return nil, addr, fmt.Errorf("locks/client: read: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	ok = true
	return resp, addr, nil
}
