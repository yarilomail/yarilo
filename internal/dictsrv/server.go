// Package dictsrv serves the dict protocol on behalf of named dicts, so the
// engines live in one process instead of every session binary (#1733).
package dictsrv

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/pkg/dict"
	"github.com/yarilomail/yarilo/pkg/dict/proxy"
)

// Server answers for the dicts it holds. An unknown name is refused by name:
// asking for a dict nobody configured is a config error, not an empty result.
type Server struct {
	dicts   map[string]dict.Dict
	metrics *Metrics
}

// New returns a server over already-opened dicts. Nil metrics are allowed for
// tests; production passes a registered set.
func New(dicts map[string]dict.Dict, metrics *Metrics) *Server {
	return &Server{dicts: dicts, metrics: metrics}
}

// observe records one operation on one named dict.
func (s *Server) observe(sess *session, op, result string, start time.Time) {
	if s.metrics == nil || sess == nil {
		return
	}
	driver := ""
	if sess.d != nil {
		driver = sess.d.Name()
	}
	s.metrics.ops.WithLabelValues(sess.name, driver, op, result).Inc()
	s.metrics.seconds.WithLabelValues(sess.name, op).Observe(time.Since(start).Seconds())
}

// Serve accepts until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("dictsrv: accept: %w", err)
		}
		go s.serveConn(ctx, conn)
	}
}

// session is one connection: one named dict, and the transactions opened on it.
type session struct {
	d     dict.Dict
	user  string
	txs   map[uint32]dict.Tx
	mu    sync.Mutex
	name  string
	valid bool
}

func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	defer conn.Close() //nolint:errcheck
	rd := bufio.NewReaderSize(conn, proxy.MaxLine)
	sess := &session{txs: map[uint32]dict.Tx{}}

	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		if len(line) < 2 {
			continue
		}
		start := time.Now()
		reply := s.handle(ctx, sess, line, conn)
		s.observe(sess, string(line[0]), resultOf(reply), start)
		if reply != "" {
			if _, werr := conn.Write([]byte(reply)); werr != nil {
				return
			}
		}
	}
}

// handle answers one command. Iterate writes its own rows, so it returns "".
func (s *Server) handle(ctx context.Context, sess *session, line string, conn net.Conn) string {
	switch line[0] {
	case proxy.OpHello:
		return s.hello(sess, line)
	case proxy.OpLookup:
		return s.lookup(ctx, sess, line)
	case proxy.OpIterate:
		s.iterate(ctx, sess, line, conn)
		return ""
	case proxy.OpBegin:
		return s.begin(ctx, sess, line)
	case proxy.OpSet, proxy.OpUnset, proxy.OpAtomicInc:
		return s.mutate(sess, line)
	case proxy.OpCommit:
		return s.commit(sess, line)
	case proxy.OpRollback:
		return s.rollback(sess, line)
	}
	return fmt.Sprintf("%c unknown command\n", proxy.ReplyFail)
}

func (s *Server) hello(sess *session, line string) string {
	_, user, name, err := proxy.ParseHello(line)
	if err != nil {
		return fmt.Sprintf("%c%s\n", proxy.ReplyFail, err)
	}
	d, ok := s.dicts[name]
	if !ok {
		slog.Warn("dictsrv: unknown dict asked for", "name", name, "user", user)
		return fmt.Sprintf("%c no such dict: %s\n", proxy.ReplyFail, name)
	}
	sess.d, sess.user, sess.name, sess.valid = d, user, name, true
	return ""
}

func (s *Server) lookup(ctx context.Context, sess *session, line string) string {
	if !sess.valid {
		return fmt.Sprintf("%c no hello\n", proxy.ReplyFail)
	}
	body := strings.TrimSuffix(line[1:], "\n")
	key, user, _ := strings.Cut(body, "\t")
	vals, found, err := sess.d.Lookup(ctx, opSet(user), key)
	switch {
	case err != nil:
		return fmt.Sprintf("%c%s\n", proxy.ReplyFail, err)
	case !found:
		return fmt.Sprintf("%c\n", proxy.ReplyNoMatch)
	case len(vals) == 1:
		return fmt.Sprintf("%c%s\n", proxy.ReplyOK, proxy.EscapeValue(vals[0]))
	}
	parts := make([]string, 0, len(vals))
	for _, v := range vals {
		parts = append(parts, proxy.EscapeValue(v))
	}
	return fmt.Sprintf("%c%s\n", proxy.ReplyMulti, strings.Join(parts, "\t"))
}

func (s *Server) iterate(ctx context.Context, sess *session, line string, conn net.Conn) {
	fail := func(msg string) {
		_, _ = conn.Write([]byte(fmt.Sprintf("%c%s\n", proxy.ReplyFail, msg)))
	}
	if !sess.valid {
		fail("no hello")
		return
	}
	f := strings.Split(strings.TrimSuffix(line[1:], "\n"), "\t")
	if len(f) < 3 {
		fail("malformed iterate")
		return
	}
	flags, err := strconv.Atoi(f[0])
	if err != nil {
		fail("malformed flags")
		return
	}
	user := ""
	if len(f) > 3 {
		user = f[3]
	}
	it, err := sess.d.Iterate(ctx, opSet(user), f[2], dict.IterFlag(flags))
	if err != nil {
		fail(err.Error())
		return
	}
	defer it.Close() //nolint:errcheck
	noValue := dict.IterFlag(flags)&dict.IterNoValue != 0
	for it.Next() {
		row := it.Key()
		if !noValue {
			var v []byte
			if vs := it.Values(); len(vs) > 0 {
				v = vs[0]
			}
			row += "\t" + proxy.EscapeValue(v)
		}
		if _, werr := conn.Write([]byte(fmt.Sprintf("%c%s\n", proxy.ReplyOK, row))); werr != nil {
			return
		}
	}
	if it.Err() != nil {
		fail(it.Err().Error())
		return
	}
	_, _ = conn.Write([]byte("\n"))
}

func (s *Server) begin(ctx context.Context, sess *session, line string) string {
	if !sess.valid {
		return fmt.Sprintf("%c no hello\n", proxy.ReplyFail)
	}
	f := strings.Split(strings.TrimSuffix(line[1:], "\n"), "\t")
	id, err := strconv.ParseUint(f[0], 10, 32)
	if err != nil {
		return fmt.Sprintf("%c malformed transaction id\n", proxy.ReplyFail)
	}
	user := ""
	if len(f) > 1 {
		user = f[1]
	}
	tx, err := sess.d.Begin(ctx, opSet(user))
	if err != nil {
		return fmt.Sprintf("%c%s\n", proxy.ReplyFail, err)
	}
	sess.mu.Lock()
	sess.txs[uint32(id)] = tx
	sess.mu.Unlock()
	return fmt.Sprintf("%c\n", proxy.ReplyOK)
}

func (s *Server) txOf(sess *session, raw string) (dict.Tx, uint32, bool) {
	id, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return nil, 0, false
	}
	sess.mu.Lock()
	tx, ok := sess.txs[uint32(id)]
	sess.mu.Unlock()
	return tx, uint32(id), ok
}

func (s *Server) mutate(sess *session, line string) string {
	f := strings.Split(strings.TrimSuffix(line[1:], "\n"), "\t")
	tx, _, ok := s.txOf(sess, f[0])
	if !ok {
		return fmt.Sprintf("%c no such transaction\n", proxy.ReplyFail)
	}
	var err error
	switch line[0] {
	case proxy.OpSet:
		if len(f) < 3 {
			return fmt.Sprintf("%c malformed set\n", proxy.ReplyFail)
		}
		err = tx.Set(f[1], proxy.UnescapeValue(f[2]))
	case proxy.OpUnset:
		err = tx.Unset(f[1])
	case proxy.OpAtomicInc:
		delta, perr := strconv.ParseInt(f[2], 10, 64)
		if perr != nil {
			return fmt.Sprintf("%c malformed delta\n", proxy.ReplyFail)
		}
		err = tx.AtomicInc(f[1], delta)
	}
	if err != nil {
		return fmt.Sprintf("%c%s\n", proxy.ReplyFail, err)
	}
	return fmt.Sprintf("%c\n", proxy.ReplyOK)
}

func (s *Server) commit(sess *session, line string) string {
	tx, id, ok := s.txOf(sess, strings.TrimSuffix(line[1:], "\n"))
	if !ok {
		return fmt.Sprintf("%c no such transaction\n", proxy.ReplyFail)
	}
	res, err := tx.Commit()
	sess.mu.Lock()
	delete(sess.txs, id)
	sess.mu.Unlock()
	switch {
	case res == dict.CommitOK:
		return fmt.Sprintf("%c\n", proxy.ReplyOK)
	case res == dict.CommitNotFound:
		return fmt.Sprintf("%c\n", proxy.ReplyNoMatch)
	case res == dict.CommitWriteUncertain:
		return fmt.Sprintf("%c%v\n", proxy.ReplyUncert, err)
	}
	return fmt.Sprintf("%c%v\n", proxy.ReplyFail, err)
}

func (s *Server) rollback(sess *session, line string) string {
	tx, id, ok := s.txOf(sess, strings.TrimSuffix(line[1:], "\n"))
	if !ok {
		return fmt.Sprintf("%c\n", proxy.ReplyOK)
	}
	err := tx.Rollback()
	sess.mu.Lock()
	delete(sess.txs, id)
	sess.mu.Unlock()
	if err != nil {
		return fmt.Sprintf("%c%s\n", proxy.ReplyFail, err)
	}
	return fmt.Sprintf("%c\n", proxy.ReplyOK)
}

func opSet(user string) *dict.OpSettings {
	if user == "" {
		return nil
	}
	return &dict.OpSettings{Username: user}
}

// resultOf names what a reply was, for the metric label: an empty reply is an
// iterate, which wrote its own rows.
func resultOf(reply string) string {
	switch {
	case reply == "":
		return "streamed"
	case reply[0] == proxy.ReplyFail:
		return "fail"
	case reply[0] == proxy.ReplyNoMatch:
		return "notfound"
	}
	return "ok"
}
