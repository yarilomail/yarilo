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

	// One connection cannot carry two commands: iterate streams untagged rows
	// to a terminator, and only COMMIT_ASYNC has an id (INTERNALS §32). So a
	// session's lookup waits out someone else's iteration unless it has its
	// own connection (#1902).
	mu    sync.Mutex
	idle  []*conn
	live  int
	limit int
	txn   uint32
}

// conn is one connection with its reader.
type conn struct {
	net  net.Conn
	rd   *bufio.Reader
	said bool // the greeting names the dict once per connection
}

// DialTimeout bounds every connect and every request; a dict that stopped
// answering fails the command rather than holding the session.
const DialTimeout = 10 * time.Second

// DefaultMaxConns bounds the connections one process keeps to one named dict.
const DefaultMaxConns = 8

// New returns a dict client for one named dict on the dict service. A non-nil
// tlsCfg is required wherever the service listens with internal mTLS.
func New(addr, name string, tlsCfg *tls.Config) *Client {
	return NewWithLimit(addr, name, tlsCfg, DefaultMaxConns)
}

// NewWithLimit is New with the connection ceiling set.
func NewWithLimit(addr, name string, tlsCfg *tls.Config, limit int) *Client {
	if limit < 1 {
		limit = 1
	}
	return &Client{addr: addr, name: name, tls: tlsCfg, limit: limit}
}

// take returns a connection to use, dialling one when none is idle. Blocks
// only when the ceiling is reached, and then only until one is returned.
func (c *Client) take(user string) (*conn, error) {
	c.mu.Lock()
	for {
		if n := len(c.idle); n > 0 {
			cn := c.idle[n-1]
			c.idle = c.idle[:n-1]
			c.mu.Unlock()
			return cn, nil
		}
		if c.live < c.limit {
			c.live++
			c.mu.Unlock()
			cn, err := c.open(user)
			if err != nil {
				c.mu.Lock()
				c.live--
				c.mu.Unlock()
				return nil, err
			}
			return cn, nil
		}
		c.mu.Unlock()
		// Waiting for a free connection is ordinary under load; waiting
		// forever is not, so the caller's deadline still bounds the command.
		time.Sleep(time.Millisecond)
		c.mu.Lock()
	}
}

// put returns a connection to the pool, or drops it when it is broken.
func (c *Client) put(cn *conn, broken bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if broken {
		if cn != nil {
			_ = cn.net.Close()
		}
		c.live--
		return
	}
	c.idle = append(c.idle, cn)
}

// open dials and greets: the greeting names the dict, never its URI.
func (c *Client) open(user string) (*conn, error) {
	nc, err := c.dial()
	if err != nil {
		return nil, fmt.Errorf("dict/proxy: dial %s: %w", c.addr, err)
	}
	if _, err := nc.Write([]byte(helloLine(0, user, c.name))); err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("dict/proxy: hello: %w", err)
	}
	return &conn{net: nc, rd: bufio.NewReaderSize(nc, MaxLine), said: true}, nil
}

// roundTrip writes one line on its own connection and reads one reply.
func (c *Client) roundTrip(ctx context.Context, user, line string) (string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		cn, err := c.take(user)
		if err != nil {
			return "", err
		}
		setDeadline(ctx, cn)
		if _, err := cn.net.Write([]byte(line)); err != nil {
			c.put(cn, true)
			continue
		}
		reply, err := cn.rd.ReadString('\n')
		if err != nil {
			c.put(cn, true)
			continue
		}
		c.put(cn, false)
		return reply, nil
	}
	return "", fmt.Errorf("dict/proxy: %s unreachable", c.addr)
}

func setDeadline(ctx context.Context, cn *conn) {
	if dl, ok := ctx.Deadline(); ok {
		_ = cn.net.SetDeadline(dl)
		return
	}
	_ = cn.net.SetDeadline(time.Now().Add(DialTimeout))
}

// Name reports the driver name, which for a proxied dict is what it is: a
// client. The engine's name lives in the dict service's config.
func (c *Client) Name() string { return "proxy" }

// dial speaks TLS where the service does: a plain dial against an mTLS listener
// fails at the first command, not at connect (#1733).
func (c *Client) dial() (net.Conn, error) {
	d := &net.Dialer{Timeout: DialTimeout}
	if c.tls == nil {
		return d.Dial("tcp", c.addr)
	}
	return tls.DialWithDialer(d, "tcp", c.addr, c.tls)
}

func userOf(set *dict.OpSettings) string {
	if set == nil {
		return ""
	}
	return set.Username
}

// Lookup implements dict.Dict.
func (c *Client) Lookup(ctx context.Context, set *dict.OpSettings, key string) ([][]byte, bool, error) {
	user := userOf(set)
	reply, err := c.roundTrip(ctx, user, fmt.Sprintf("%c%s\t%s\n", OpLookup, key, user))
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
	user := userOf(set)
	cn, err := c.take(user)
	if err != nil {
		return nil, err
	}
	setDeadline(ctx, cn)
	line := fmt.Sprintf("%c%d\t%d\t%s\t%s\n", OpIterate, int(flags), 0, path, user)
	if _, err := cn.net.Write([]byte(line)); err != nil {
		c.put(cn, true)
		return nil, fmt.Errorf("dict/proxy: iterate: %w", err)
	}
	return &iterator{c: c, cn: cn, noValue: flags&dict.IterNoValue != 0}, nil
}

// iterator holds the client lock for its lifetime: the rows are a stream on the
// one connection, so another command may not interleave with them.
type iterator struct {
	c       *Client
	cn      *conn
	noValue bool
	key     string
	vals    [][]byte
	err     error
	done    bool
	broken  bool
}

func (it *iterator) Next() bool {
	if it.done {
		return false
	}
	line, err := it.cn.rd.ReadString('\n')
	if err != nil {
		it.err = fmt.Errorf("dict/proxy: iterate read: %w", err)
		it.broken = true
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
	if it.cn == nil {
		return it.err
	}
	if !it.done {
		// Drain to the terminator, or the connection would carry these rows
		// into whoever takes it next.
		for it.Next() {
		}
	}
	it.c.put(it.cn, it.broken)
	it.cn = nil
	return it.err
}

// Begin implements dict.Dict.
func (c *Client) Begin(ctx context.Context, set *dict.OpSettings) (dict.Tx, error) {
	user := userOf(set)
	c.mu.Lock()
	c.txn++
	id := c.txn
	c.mu.Unlock()
	if _, err := c.roundTrip(ctx, user, fmt.Sprintf("%c%d\t%s\n", OpBegin, id, user)); err != nil {
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
	_, err := t.c.roundTrip(t.ctx, t.user, line)
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
	reply, err := t.c.roundTrip(t.ctx, t.user, fmt.Sprintf("%c%d\n", OpCommit, t.id))
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
	for _, cn := range c.idle {
		_ = cn.net.Close()
	}
	c.idle = nil
	c.live = 0
	return nil
}
