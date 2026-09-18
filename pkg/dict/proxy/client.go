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

// Client is a dict.Dict served by yarilo-dict. Connections are pooled: an
// iteration streams untagged rows to a terminator and only COMMIT_ASYNC carries
// an id, so nothing may share a connection with one (#1902).
type Client struct {
	addr string
	name string
	tls  *tls.Config

	// sem bounds connections; mu guards the idle set and the id counter.
	sem  chan struct{}
	mu   sync.Mutex
	idle []*conn
	txn  uint32
}

// conn is one connection with its reader.
type conn struct {
	net net.Conn
	rd  *bufio.Reader
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
	return &Client{addr: addr, name: name, tls: tlsCfg, sem: make(chan struct{}, limit)}
}

// Name reports the driver name, which for a proxied dict is what it is: a
// client. The engine's name lives in the dict service's config.
func (c *Client) Name() string { return "proxy" }

// take returns a connection, waiting for a free slot no longer than the
// caller's deadline: a command that was abandoned must not hold one.
func (c *Client) take(ctx context.Context, user string) (*conn, error) {
	// Cancellation is honoured with or without a deadline; the timer only adds
	// a bound where the caller set none.
	var timer *time.Timer
	if _, ok := ctx.Deadline(); !ok {
		timer = time.NewTimer(DialTimeout)
		defer timer.Stop()
	}
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timerC(timer):
		return nil, fmt.Errorf("dict/proxy: no free connection to %s", c.addr)
	}
	c.mu.Lock()
	if n := len(c.idle); n > 0 {
		cn := c.idle[n-1]
		c.idle = c.idle[:n-1]
		c.mu.Unlock()
		return cn, nil
	}
	c.mu.Unlock()
	cn, err := c.open(user)
	if err != nil {
		<-c.sem
		return nil, err
	}
	return cn, nil
}

// timerC is nil for a nil timer, which a select reads as "never".
func timerC(t *time.Timer) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}

// put returns a connection, or drops it and everything else idle: connections
// are dialled together and a restart kills them together (#1902).
func (c *Client) put(cn *conn, broken bool) {
	c.mu.Lock()
	if broken {
		if cn != nil {
			_ = cn.net.Close()
		}
		stale := c.idle
		c.idle = nil
		c.mu.Unlock()
		for _, s := range stale {
			_ = s.net.Close()
		}
		<-c.sem
		return
	}
	c.idle = append(c.idle, cn)
	c.mu.Unlock()
	<-c.sem
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
	return &conn{net: nc, rd: bufio.NewReaderSize(nc, MaxLine)}, nil
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

// exchange writes one line on cn and reads one reply.
func exchange(ctx context.Context, cn *conn, line string) (string, error) {
	setDeadline(ctx, cn)
	if _, err := cn.net.Write([]byte(line)); err != nil {
		return "", err
	}
	return cn.rd.ReadString('\n')
}

func setDeadline(ctx context.Context, cn *conn) {
	if dl, ok := ctx.Deadline(); ok {
		_ = cn.net.SetDeadline(dl)
		return
	}
	_ = cn.net.SetDeadline(time.Now().Add(DialTimeout))
}

// roundTrip runs one command on a pooled connection, re-dialling once: the
// service restarts like any other pod, and one restart is not a failure.
func (c *Client) roundTrip(ctx context.Context, user, line string) (string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		cn, err := c.take(ctx, user)
		if err != nil {
			return "", err
		}
		reply, err := exchange(ctx, cn, line)
		if err != nil {
			c.put(cn, true)
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			continue
		}
		c.put(cn, false)
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
	cn, err := c.take(ctx, user)
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

// iterator owns its connection for its lifetime: the rows are an untagged
// stream, so nothing else may use that connection until it ends.
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
	// The service holds a transaction on the connection it began on, so this
	// one is held until Commit or Rollback (#1902).
	cn, err := c.take(ctx, user)
	if err != nil {
		return nil, err
	}
	if _, err := exchange(ctx, cn, fmt.Sprintf("%c%d\t%s\n", OpBegin, id, user)); err != nil {
		c.put(cn, true)
		return nil, fmt.Errorf("dict/proxy: begin: %w", err)
	}
	return &tx{c: c, cn: cn, id: id, ctx: ctx, user: user}, nil
}

type tx struct {
	c    *Client
	cn   *conn
	id   uint32
	ctx  context.Context
	user string
	done bool
}

// send runs one mutation on the transaction's own connection.
func (t *tx) send(line string) error {
	if t.cn == nil {
		return fmt.Errorf("dict/proxy: transaction is finished")
	}
	reply, err := exchange(t.ctx, t.cn, line)
	if err != nil {
		t.release(true)
		return fmt.Errorf("dict/proxy: transaction: %w", err)
	}
	if len(reply) > 0 && reply[0] == ReplyFail {
		return fmt.Errorf("dict/proxy: %s", strings.TrimSuffix(reply[1:], "\n"))
	}
	return nil
}

// release returns the transaction's connection to the pool.
func (t *tx) release(broken bool) {
	if t.cn == nil {
		return
	}
	t.c.put(t.cn, broken)
	t.cn = nil
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
	if t.cn == nil {
		return dict.CommitFailed, fmt.Errorf("dict/proxy: transaction is finished")
	}
	reply, err := exchange(t.ctx, t.cn, fmt.Sprintf("%c%d\n", OpCommit, t.id))
	if err != nil {
		t.release(true)
		return dict.CommitFailed, fmt.Errorf("dict/proxy: commit: %w", err)
	}
	t.release(false)
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
	err := t.send(fmt.Sprintf("%c%d\n", OpRollback, t.id))
	t.release(err != nil)
	return err
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
	return nil
}
