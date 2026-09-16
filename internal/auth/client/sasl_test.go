package client

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
)

// countingAuthServer answers a relayed SCRAM exchange and counts what it was
// asked: connections, handshakes and commands, each on its own.
func countingAuthServer(t *testing.T, mechs []string) (addr string, conns, handshakes, auths, cancels *atomic.Int64) {
	t.Helper()
	conns, handshakes, auths, cancels = &atomic.Int64{}, &atomic.Int64{}, &atomic.Int64{}, &atomic.Int64{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			conns.Add(1)
			go func() {
				defer c.Close() //nolint:errcheck
				rd := bufio.NewReader(c)
				fmt.Fprintf(c, "VERSION\t1\t0\n")
				for _, m := range mechs {
					fmt.Fprintf(c, "MECH\t%s\tactive\n", m)
				}
				fmt.Fprintf(c, "DONE\n")
				for {
					line, rerr := rd.ReadString('\n')
					if rerr != nil {
						return
					}
					fields := strings.Split(strings.TrimRight(line, "\n"), "\t")
					switch fields[0] {
					case "VERSION":
						handshakes.Add(1)
					case "AUTH":
						auths.Add(1)
						fmt.Fprintf(c, "CONT\t%s\t%s\n", fields[1],
							base64.StdEncoding.EncodeToString([]byte("r=nonce,s=c2FsdA==,i=4096"))) //nolint:errcheck
					case "CONT":
						fmt.Fprintf(c, "OK\t%s\tuser=alice\tresp=%s\n", fields[1],
							base64.StdEncoding.EncodeToString([]byte("v=signature"))) //nolint:errcheck
					case "CANCEL":
						cancels.Add(1)
						fmt.Fprintf(c, "OK\t%s\n", fields[1]) //nolint:errcheck
					}
				}
			}()
		}
	}()
	return ln.Addr().String(), conns, handshakes, auths, cancels
}

// The list a session advertises comes from the handshake and is read once, for
// the life of the connection: re-reading it per login would double the traffic
// the service carries under a login storm (#1733).
func TestTheMechanismListIsReadOncePerConnection(t *testing.T) {
	addr, conns, handshakes, auths, _ := countingAuthServer(t, []string{"PLAIN", "LOGIN", "SCRAM-SHA-256"})
	c, err := Dial(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close() //nolint:errcheck

	const logins = 5
	for i := 0; i < logins; i++ {
		if got := c.Mechanisms(); len(got) != 3 {
			t.Fatalf("Mechanisms() = %v, want the three announced", got)
		}
		x, step, serr := c.BeginSASL("SCRAM-SHA-256", "imap", "", "s1", nil, []byte("n,,n=alice,r=nonce"))
		if serr != nil {
			t.Fatalf("begin: %v", serr)
		}
		if step.Done {
			t.Fatalf("login %d finished at the first step: %+v", i, step)
		}
		final, ferr := x.Next([]byte("c=biws,r=nonce,p=proof"))
		if ferr != nil {
			t.Fatalf("next: %v", ferr)
		}
		if !final.Done || final.Result == nil {
			t.Fatalf("login %d did not finish: %+v", i, final)
		}
		if string(final.Final) != "v=signature" {
			t.Errorf("server-final = %q, want the signature the service sent", final.Final)
		}
	}

	if n := conns.Load(); n != 1 {
		t.Errorf("%d logins opened %d connections, want 1", logins, n)
	}
	if n := handshakes.Load(); n != 1 {
		t.Errorf("%d logins cost %d handshakes, want the one at dial", logins, n)
	}
	if got, want := auths.Load(), int64(logins); got != want {
		t.Errorf("%d logins cost %d AUTH commands, want %d", logins, got, want)
	}
}

// An abandoned exchange is cancelled by the session, so the service frees the
// id at once instead of holding it until the deadline (#1733).
func TestAnAbandonedRelayIsCancelled(t *testing.T) {
	addr, _, _, _, cancels := countingAuthServer(t, []string{"SCRAM-SHA-256"})
	c, err := Dial(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close() //nolint:errcheck

	srv := NewRelayServer(c, "SCRAM-SHA-256", "imap", "", "s1", nil)
	if _, _, nerr := srv.Next([]byte("n,,n=alice,r=nonce")); nerr != nil {
		t.Fatalf("first step: %v", nerr)
	}
	srv.Cancel()
	srv.Cancel() // idempotent: a teardown path may always call it

	if n := cancels.Load(); n != 1 {
		t.Errorf("the service saw %d CANCEL commands, want exactly one", n)
	}
}
