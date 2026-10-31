package login

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	goSmtp "github.com/emersion/go-smtp"

	authclient "github.com/yarilomail/yarilo/v2/internal/auth/client"
	"github.com/yarilomail/yarilo/v2/internal/loginproto"
)

// preambleLn strips the preamble line as the backend's PreambleListener does
// and hands go-smtp what is left of the stream.
type preambleLn struct{ net.Listener }

type bufConn struct {
	net.Conn
	rd *bufio.Reader
}

func (c bufConn) Read(p []byte) (int, error) { return c.rd.Read(p) }

func (l preambleLn) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	rd := bufio.NewReader(c)
	if _, err := loginproto.Parse(rd); err != nil {
		c.Close()
		return nil, err
	}
	return bufConn{Conn: c, rd: rd}, nil
}

type nopBackend struct{}

func (nopBackend) NewSession(*goSmtp.Conn) (goSmtp.Session, error) { return nopSession{}, nil }

type nopSession struct{}

func (nopSession) Mail(string, *goSmtp.MailOptions) error { return nil }
func (nopSession) Rcpt(string, *goSmtp.RcptOptions) error { return nil }
func (nopSession) Data(io.Reader) error                   { return nil }
func (nopSession) Reset()                                 {}
func (nopSession) Logout() error                          { return nil }

// The session opens with nothing of the backend's EHLO reply left over: the
// next reply the client reads is the reply to its own next command (#2271).
func TestSubmissionSessionLeavesNoEHLOReplyBehind(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := goSmtp.NewServer(nopBackend{})
	srv.Domain = "backend.test"
	go srv.Serve(preambleLn{ln}) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })

	s := &Server{opts: testOpts(Options{Protocol: ProtocolSubmission, BackendAddr: ln.Addr().String()})}
	pre := &preamble{ehloLine: "EHLO client.example\r\n"}
	bs, _, err := s.openBackendSession(pre, &authclient.AuthResult{Token: "t"}, "u@example.com", "submission",
		ln.Addr().String(), "192.0.2.1", "sid", time.Now().Add(5*time.Second), slog.Default())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer bs.conn.Close()
	bs.conn.SetDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
	if _, err := io.WriteString(bs.conn, "NOOP\r\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bs.rd.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "250 ") || strings.Contains(line, "Hello") {
		t.Errorf("the reply to NOOP reads %q: the EHLO reply was left for the client", strings.TrimSpace(line))
	}
}

// A preamble field cannot end the line: the backend reads one line and
// nothing of it reaches the SMTP server.
func TestPreambleFieldsCannotBreakTheLine(t *testing.T) {
	line := loginproto.Preamble{User: "u\tx", Helo: "EHLO client.example\r\n"}.Format()
	if strings.Count(line, "\n") != 1 || strings.Contains(line, "\r") || strings.Count(line, "\t") != 2 {
		t.Errorf("preamble %q: want one line, no CR, a tab only between fields", line)
	}
}

func TestHeloDomain(t *testing.T) {
	for line, want := range map[string]string{
		"EHLO client.example\r\n": "client.example",
		"HELO [192.0.2.1]":        "[192.0.2.1]",
		"":                        "",
		"EHLO":                    "",
	} {
		if got := heloDomain(line); got != want {
			t.Errorf("heloDomain(%q) = %q, want %q", line, got, want)
		}
	}
}

// A backend whose reply to the login's EHLO is not 250 does not get the
// session: its reply belongs to some other command.
func TestSubmissionSessionRefusesANon250EHLOReply(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		rd := bufio.NewReader(c)
		_, _ = loginproto.Parse(rd)
		_, _ = io.WriteString(c, "220 backend.test ESMTP\r\n")
		_, _ = rd.ReadString('\n')
		_, _ = io.WriteString(c, "500 5.5.2 Error: bad syntax\r\n")
		time.Sleep(time.Second)
	}()
	s := &Server{opts: testOpts(Options{Protocol: ProtocolSubmission, BackendAddr: ln.Addr().String()})}
	_, _, err = s.openBackendSession(&preamble{ehloLine: "EHLO client.example\r\n"}, &authclient.AuthResult{Token: "t"},
		"u@example.com", "submission", ln.Addr().String(), "192.0.2.1", "sid", time.Now().Add(3*time.Second), slog.Default())
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("open = %v, want a refusal naming the 500", err)
	}
}
