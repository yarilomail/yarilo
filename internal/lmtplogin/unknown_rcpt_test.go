package lmtplogin

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
	authclient "github.com/yarilomail/yarilo/pkg/authclient"
	"github.com/yarilomail/yarilo/pkg/sqlvars"
)

// startLookupCountingDirector answers every LOOKUP with a HOST and counts them.
func startLookupCountingDirector(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("director listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	var lookups atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				fmt.Fprintf(conn, "VERSION\tyarilo-director\t1\t0\nDONE\n")
				rd := bufio.NewReader(conn)
				for {
					line, err := rd.ReadString('\n')
					if err != nil {
						return
					}
					f := strings.Split(strings.TrimRight(line, "\r\n"), "\t")
					if f[0] == "LOOKUP" && len(f) >= 2 {
						lookups.Add(1)
						fmt.Fprintf(conn, "HOST\t%s\t10.0.0.9\t24\n", f[1])
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), &lookups
}

// A recipient userdb does not know is refused at RCPT, before the director is
// asked: a LOOKUP would pin a name that does not exist in every director.
func TestAnUnknownRecipientIsRefusedBeforeTheDirector(t *testing.T) {
	down, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	downAddr := down.Addr().String()
	down.Close()

	known := startTestAuthWithUserdb(t, fakeUserdb{
		"alice@example.com": {Username: "alice@example.com"},
	})
	cases := []struct {
		name        string
		auth        string
		rcpt        string
		reply       string
		wantLookups int32
	}{
		{"unknown", known, "nobody@example.com", "550 5.1.1 ", 0},
		{"known", known, "alice@example.com", "250 ", 1},
		{"userdb unreachable", downAddr, "alice@example.com", "451 4.3.0 ", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			director, lookups := startLookupCountingDirector(t)
			proxy := startLMTPLogin(t, testOpts(Options{
				Hostname: "test.local", DirectorAddr: director, AuthMasterAddr: tc.auth,
			}))
			mta := dialMTA(t, proxy)
			mta.lmtpHandshake(t)
			mta.mailFrom(t, "sender@example.com")
			if got := mta.rcptReply(t, tc.rcpt); !strings.HasPrefix(got, tc.reply) {
				t.Errorf("RCPT TO:<%s> answered %q, want %q", tc.rcpt, got, tc.reply)
			}
			if n := lookups.Load(); n != tc.wantLookups {
				t.Errorf("the director got %d LOOKUPs, want %d", n, tc.wantLookups)
			}
		})
	}
}

// lmtpOff is a userdb whose rule on %s turns delivery off for ruled@example.com.
type lmtpOff struct{}

func (lmtpOff) Lookup(u string) (*protocol.UserInfo, error) {
	if u == "nobody@example.com" {
		return nil, nil
	}
	return &protocol.UserInfo{Username: u}, nil
}

func (l lmtpOff) LookupFor(u string, v sqlvars.Vars) (*protocol.UserInfo, error) {
	if u == "ruled@example.com" && v.Service == "lmtp" {
		return nil, nil
	}
	return l.Lookup(u)
}

func (lmtpOff) UsesRequest() bool { return true }

// The lookup names lmtp, so a recipient whose delivery a userdb rule turns off
// gets the same reply as one that does not exist (#2227), pooled or not.
func TestARuledRecipientIsRefusedAsAnUnknownOne(t *testing.T) {
	auth := startTestAuthWithUserdb(t, lmtpOff{})
	for _, pooled := range []bool{false, true} {
		t.Run(fmt.Sprintf("pooled=%v", pooled), func(t *testing.T) {
			director, lookups := startLookupCountingDirector(t)
			opts := Options{Hostname: "test.local", DirectorAddr: director, AuthMasterAddr: auth}
			if pooled {
				opts.AuthMasterPool = authclient.NewPool(auth, nil, 2, time.Minute)
			}
			proxy := startLMTPLogin(t, testOpts(opts))
			mta := dialMTA(t, proxy)
			mta.lmtpHandshake(t)
			mta.mailFrom(t, "sender@example.com")
			ruled := mta.rcptReply(t, "ruled@example.com")
			unknown := mta.rcptReply(t, "nobody@example.com")
			if ruled != unknown || !strings.HasPrefix(ruled, "550 5.1.1 ") {
				t.Errorf("ruled recipient answered %q, unknown one %q; want the same 550 5.1.1", ruled, unknown)
			}
			if n := lookups.Load(); n != 0 {
				t.Errorf("the director got %d LOOKUPs, want 0", n)
			}
		})
	}
}
