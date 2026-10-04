package client_test

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yarilomail/yarilo/internal/auth/authtest"
	authrelay "github.com/yarilomail/yarilo/internal/auth/client"
	"github.com/yarilomail/yarilo/internal/auth/protocol"
)

// countingAuth accepts alice with one password and counts what reaches it.
type countingAuth struct {
	pass  string
	calls atomic.Int32
}

func (a *countingAuth) Authenticate(username, password, _, _ string) (*protocol.AuthResponse, error) {
	a.calls.Add(1)
	if username == "alice" && password == a.pass {
		return &protocol.AuthResponse{Result: protocol.AuthOK, Username: username}, nil
	}
	return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
}

// A password holding a line break and a TAB is one request, not two.
func TestAPasswordCannotSplitTheRequest(t *testing.T) {
	auth := &countingAuth{pass: "pa\nAUTH\t99\tPLAIN\tresp=x\tss"}
	c := authtest.RelayTo(t, auth)
	res, err := c.Authenticate("alice", auth.pass, "imap", "", "")
	if err != nil || res == nil {
		t.Fatalf("Authenticate = %v, %v; want OK", res, err)
	}
	if n := auth.calls.Load(); n != 1 {
		t.Fatalf("the service ran %d authentications, want 1", n)
	}
}

// A name that would end a field is refused before anything is sent.
func TestAnUnsafeNameNeverReachesTheWire(t *testing.T) {
	auth := &countingAuth{pass: "secret"}
	c := authtest.RelayTo(t, auth)
	for _, name := range []string{"ali\tce", "ali\nce", "ali\x00ce"} {
		if _, err := c.Authenticate(name, "secret", "imap", "", ""); !errors.Is(err, authrelay.ErrAuthFailed) {
			t.Errorf("%q: err = %v, want ErrAuthFailed", name, err)
		}
	}
	if n := auth.calls.Load(); n != 0 {
		t.Fatalf("the service ran %d authentications, want none", n)
	}
}

// A reply for an id this client did not issue is dropped, not delivered.
func TestAReplyForAnotherIDIsDropped(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close() //nolint:errcheck
		rd := bufio.NewReader(conn)
		fmt.Fprint(conn, "VERSION\t1\t0\nMECH\tPLAIN\tplaintext\nDONE\n")
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			f := strings.Split(strings.TrimRight(line, "\n"), "\t")
			if f[0] != "AUTH" {
				continue
			}
			fmt.Fprintf(conn, "OK\t%s9\tuser=mallory\n", f[1])
			fmt.Fprintf(conn, "FAIL\t%s\n", f[1])
		}
	}()
	c, err := authrelay.Dial(ln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() }) //nolint:errcheck
	if res, err := c.Authenticate("alice", "secret", "imap", "", ""); !errors.Is(err, authrelay.ErrAuthFailed) {
		t.Fatalf("Authenticate = %+v, %v; want the FAIL addressed to this request", res, err)
	}
}
