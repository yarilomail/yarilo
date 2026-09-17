package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/pkg/dict"
)

// Client is a dict.Dict served by yarilo-dict. One connection per dict name,
// re-dialled on loss: a session must get an error, never a hang (#1733).
type Client struct {
	addr string
	name string
	tls  *tls.Config

	mu   sync.Mutex
	conn net.Conn
	rd   *bufio.Reader
	txn  uint32
}

// DialTimeout bounds every connect and every request; a dict that stopped
// answering fails the command rather than holding the session.
const DialTimeout = 10 * time.Second

// New returns a dict client for one named dict on the dict service. A non-nil
// tlsCfg is required wherever the service listens with internal mTLS.
func New(addr, name string, tlsCfg *tls.Config) *Client {
	return &Client{addr: addr, name: name, tls: tlsCfg}
}

// Name reports the driver name, which for a proxied dict is what it is: a
// client. The engine's name lives in the dict service's config.
func (c *Client) Name() string { return "proxy" }

func (c *Client) ensure(user string) error {
	if c.conn != nil {
		return nil
	}
	conn, err := c.dial()
	if err != nil {
		return fmt.Errorf("dict/proxy: dial %s: %w", c.addr, err)
	}
	c.conn = conn
	c.rd = bufio.NewReaderSize(conn, MaxLine)
	if _, err := conn.Write([]byte(helloLine(0, user, c.name))); err != nil {
		c.drop()
		return fmt.Errorf("dict/proxy: hello: %w", err)
	}
	return nil
}

// dial speaks TLS where the service does: a plain dial against an mTLS listener
// fails at the first command, not at connect (#1733).
func (c *Client) dial() (net.Conn, error) {
	d := &net.Dialer{Timeout: DialTimeout}
	if c.tls == nil {
		return d.Dial("tcp", c.addr)
	}
	return tls.DialWithDialer(d, "tcp", c.addr, c.tls)
}

func (c *Client) drop() {
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
		c.rd = nil
	}
}

// request writes one line and reads one reply, re-dialling once: the service is
// restarted like any other pod, and one restart is not an error to the caller.
func (c *Client) request(ctx context.Context, user, line string) (string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if err := c.ensure(user); err != nil {
			return "", err
		}
		if dl, ok := ctx.Deadline(); ok {
			_ = c.conn.SetDeadline(dl)
		} else {
			_ = c.conn.SetDeadline(time.Now().Add(DialTimeout))
		}
		if _, err := c.conn.Write([]byte(line)); err != nil {
			c.drop()
			continue
		}
		reply, err := c.rd.ReadString('\n')
		if err != nil {
			c.drop()
			continue
		}
		return reply, nil
	}
	return "", fmt.Errorf("dict/proxy: %s unreachable", c.addr)
}

func userOf(set *dict.OpSettings) string {
	if set == nil {
		return ""
	}
	return set.Username
}

// Lookup implements dict.Dict.
func (c *Client) Lookup(ctx context.Context, set *dict.OpSettings, key string) ([][]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	user := userOf(set)
	reply, err := c.request(ctx, user, fmt.Sprintf("%c%s\t%s\n", OpLookup, key, user))
	if err != nil {
		return nil, false, err
	}
	reply = strings.TrimSuffix(reply, "\n")
	switch {
	case reply == "":
		return nil, false, fmt.Errorf("dict/proxy: empty reply")
	case reply[0] == ReplyNoMatch:
		return nil, false, nil
	case reply[0] == ReplyOK:
		return [][]byte{UnescapeValue(reply[1:])}, true, nil
	case reply[0] == ReplyMulti:
		parts := strings.Split(reply[1:], "\t")
		out := make([][]byte, 0, len(parts))
		for _, p := range parts {
			out = append(out, UnescapeValue(p))
		}
		return out, true, nil
	case reply[0] == ReplyFail:
		return nil, false, fmt.Errorf("dict/proxy: lookup: %s", reply[1:])
	}
	return nil, false, fmt.Errorf("dict/proxy: unexpected reply %q", reply)
}

// Iterate implements dict.Dict. Rows stream until the empty line.
func (c *Client) Iterate(ctx context.Context, set *dict.OpSettings, path string, flags dict.IterFlag) (dict.Iterator, error) {
	c.mu.Lock()
	user := userOf(set)
	line := fmt.Sprintf("%c%d\t%d\t%s\t%s\n", OpIterate, int(flags), 0, path, user)
	if err := c.ensure(user); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	if _, err := c.conn.Write([]byte(line)); err != nil {
		c.drop()
		c.mu.Unlock()
		return nil, fmt.Errorf("dict/proxy: iterate: %w", err)
	}
	return &iterator{c: c, noValue: flags&dict.IterNoValue != 0}, nil
}

// iterator holds the client lock for its lifetime: the rows are a stream on the
// one connection, so another command may not interleave with them.
type iterator struct {
	c       *Client
	noValue bool
	key     string
	vals    [][]byte
	err     error
	done    bool
}

func (it *iterator) Next() bool {
	if it.done {
		return false
	}
	line, err := it.c.rd.ReadString('\n')
	if err != nil {
		it.err = fmt.Errorf("dict/proxy: iterate read: %w", err)
		it.c.drop()
		it.done = true
		return false
	}
	line = strings.TrimSuffix(line, "\n")
	if line == "" {
		it.done = true
		return false
	}
	if line[0] == ReplyFail {
		it.err = fmt.Errorf("dict/proxy: iterate: %s", line[1:])
		it.done = true
		return false
	}
	body := line[1:]
	if it.noValue {
		it.key, it.vals = body, nil
		return true
	}
	k, v, _ := strings.Cut(body, "\t")
	it.key = k
	it.vals = [][]byte{UnescapeValue(v)}
	return true
}

func (it *iterator) Key() string      { return it.key }
func (it *iterator) Values() [][]byte { return it.vals }
func (it *iterator) Err() error       { return it.err }

func (it *iterator) Close() error {
	if !it.done {
		// Drain to the terminator, or the next command would read these rows.
		for it.Next() {
		}
	}
	it.c.mu.Unlock()
	return it.err
}

// Begin implements dict.Dict.
func (c *Client) Begin(ctx context.Context, set *dict.OpSettings) (dict.Tx, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	user := userOf(set)
	c.txn++
	id := c.txn
	if _, err := c.request(ctx, user, fmt.Sprintf("%c%d\t%s\n", OpBegin, id, user)); err != nil {
		return nil, err
	}
	return &tx{c: c, id: id, ctx: ctx, user: user}, nil
}

type tx struct {
	c    *Client
	id   uint32
	ctx  context.Context
	user string
	done bool
}

func (t *tx) send(line string) error {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	_, err := t.c.request(t.ctx, t.user, line)
	return err
}

func (t *tx) Set(key string, value []byte) error {
	return t.send(fmt.Sprintf("%c%d\t%s\t%s\n", OpSet, t.id, key, EscapeValue(value)))
}

func (t *tx) Unset(key string) error {
	return t.send(fmt.Sprintf("%c%d\t%s\n", OpUnset, t.id, key))
}

func (t *tx) AtomicInc(key string, delta int64) error {
	return t.send(fmt.Sprintf("%c%d\t%s\t%s\n", OpAtomicInc, t.id, key, strconv.FormatInt(delta, 10)))
}

func (t *tx) Commit() (dict.CommitResult, error) {
	if t.done {
		return dict.CommitOK, nil
	}
	t.done = true
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	reply, err := t.c.request(t.ctx, t.user, fmt.Sprintf("%c%d\n", OpCommit, t.id))
	if err != nil {
		return dict.CommitFailed, err
	}
	reply = strings.TrimSuffix(reply, "\n")
	switch {
	case reply == "":
		return dict.CommitFailed, fmt.Errorf("dict/proxy: empty commit reply")
	case reply[0] == ReplyOK:
		return dict.CommitOK, nil
	case reply[0] == ReplyNoMatch:
		return dict.CommitNotFound, nil
	case reply[0] == ReplyUncert:
		return dict.CommitWriteUncertain, fmt.Errorf("dict/proxy: commit uncertain: %s", reply[1:])
	}
	return dict.CommitFailed, fmt.Errorf("dict/proxy: commit: %s", reply[1:])
}

func (t *tx) Rollback() error {
	if t.done {
		return nil
	}
	t.done = true
	return t.send(fmt.Sprintf("%c%d\n", OpRollback, t.id))
}

// ExpireScan is the service's business: it holds the engine that knows what
// expired.
func (c *Client) ExpireScan(ctx context.Context) error { return nil }

// Wait implements dict.Dict; every request here is synchronous.
func (c *Client) Wait(ctx context.Context) error { return nil }

// Close drops the connection to the service.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drop()
	return nil
}
