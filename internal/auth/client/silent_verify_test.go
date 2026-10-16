package client

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// silentThenAnswering: the first connection shakes hands and then never answers,
// as a replaced auth pod's; later ones answer VERIFY and CONSUME at once.
func silentThenAnswering(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var conns atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n := conns.Add(1)
			go func(c net.Conn) {
				defer c.Close()
				rd := bufio.NewReader(c)
				_, _ = rd.ReadString('\n')
				fmt.Fprint(c, "VERSION\t1\t0\nSPID\t1\nDONE\n")
				for {
					line, err := rd.ReadString('\n')
					if err != nil {
						return
					}
					f := strings.Split(strings.TrimRight(line, "\n"), "\t")
					switch {
					case n == 1:
					case f[0] == "VERIFY":
						fmt.Fprintf(c, "OK\t%s\tuser=u52@d00002.test\tsession=s1\tservice=imap\n", f[1])
					case f[0] == "CONSUME":
						fmt.Fprintf(c, "OK\t%s\n", f[1])
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

// VERIFY or CONSUME on a silent connection fails within its own bound, not the
// request timeout, and drops it: the next goes over a fresh one at once (#2262).
func TestASilentVerifyFailsFastAndTheNextOneRedials(t *testing.T) {
	for _, op := range []struct {
		name string
		call func(c *Client) error
	}{
		{"VERIFY", func(c *Client) error { _, _, _, err := c.Verify("tok", "u52@d00002.test", "s1"); return err }},
		{"CONSUME", func(c *Client) error { return c.Consume("tok") }},
	} {
		t.Run(op.name, func(t *testing.T) {
			c, err := New(silentThenAnswering(t), nil, Options{
				RequestTimeout: 10 * time.Second,
				VerifyTimeout:  300 * time.Millisecond,
				DialTimeout:    time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()

			start := time.Now()
			if err := op.call(c); err == nil {
				t.Fatal("a request nobody answered succeeded")
			}
			if took := time.Since(start); took > 2*time.Second {
				t.Fatalf("on a silent connection it took %v; a session waits that long for its greeting", took.Round(time.Millisecond))
			}
			start = time.Now()
			if err := op.call(c); err != nil {
				t.Fatalf("the next one, with auth answering again, failed after %v: %v", time.Since(start).Round(time.Millisecond), err)
			}
			if took := time.Since(start); took > time.Second {
				t.Errorf("the next one took %v: it waited on the silent connection", took.Round(time.Millisecond))
			}
		})
	}
}
