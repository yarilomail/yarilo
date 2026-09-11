package login

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

// startPOP3Backend accepts a proxied session, reads the preamble, and answers
// with first -- the one line the proxy reads before it replies to the client.
func startPOP3Backend(t *testing.T, first string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				// One line: the preamble is a single LF-terminated record.
				if _, err := bufio.NewReader(c).ReadString('\n'); err != nil {
					return
				}
				c.Write([]byte(first)) //nolint:errcheck
			}(c)
		}
	}()
	return ln.Addr().String()
}

// The reply to PASS is the backend's own answer (#1776).
//
// The backend decides after the preamble whether the session exists at all --
// a session lock it could not take -- and the client must read that where the
// protocol puts it, not a success this pod invented and a refusal one command
// later.
func TestThePASSReplyIsTheBackendsOwnAnswer(t *testing.T) {
	const refusal = "-ERR [IN-USE] mailbox already in use, try again later\r\n"
	cases := []struct {
		name  string
		first string
		want  string
		deny  string
	}{
		{"refused", refusal, "-ERR [IN-USE] mailbox already in use, try again later", "+OK"},
		{"accepted", "+OK yarilo POP3 server ready\r\n", "+OK Logged in", "-ERR"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wardenAddr, _ := startWardenWithHandle(t)
			authAddr := startOKAuth(t)
			s := &Server{
				opts: Options{
					Protocol:    ProtocolPOP3,
					AuthAddr:    authAddr,
					WardenAddr:  wardenAddr,
					BackendAddr: startPOP3Backend(t, c.first),
				},
				sessions: make(map[string][]*liveSession),
			}
			t.Cleanup(func() {
				if s.wardenPool != nil {
					s.wardenPool.Close()
				}
			})

			srv, cli := pipePair(t)
			go s.handleConn(srv)
			cli.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
			crd := bufio.NewReader(cli)
			crd.ReadString('\n') //nolint:errcheck // the proxy's own greeting

			cli.Write([]byte("USER alice\r\n"))  //nolint:errcheck
			crd.ReadString('\n')                 //nolint:errcheck
			cli.Write([]byte("PASS secret\r\n")) //nolint:errcheck
			got, err := crd.ReadString('\n')
			if err != nil {
				t.Fatalf("read the PASS reply: %v", err)
			}
			got = strings.TrimRight(got, "\r\n")
			if got != c.want {
				t.Errorf("the reply to PASS is %q, want %q", got, c.want)
			}
			if strings.HasPrefix(got, c.deny) {
				t.Errorf("the reply to PASS starts with %q, which is the answer this pod made up", c.deny)
			}
		})
	}
}
