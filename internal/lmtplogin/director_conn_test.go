package lmtplogin

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
)

// startCountingDirector answers every LOOKUP with a HOST and counts accepted
// connections; dropAfterReply closes each connection after its first answer.
func startCountingDirector(t *testing.T, dropAfterReply bool) (string, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("director listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	var accepted atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
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
					if f[0] != "LOOKUP" || len(f) < 2 {
						continue
					}
					fmt.Fprintf(conn, "HOST\t%s\t10.0.0.9\t24\n", f[1])
					if dropAfterReply {
						return
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), &accepted
}

// One MTA session keeps one director connection (#2149); a connection the
// director dropped is redialled rather than failing the recipient.
func TestDirectorConnectionPerSession(t *testing.T) {
	cases := []struct {
		name     string
		drop     bool
		wantDial int32
	}{
		{"two RCPTs, one handshake", false, 1},
		{"dropped connection redialled", true, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			addr, accepted := startCountingDirector(t, c.drop)
			s := &session{opts: testOpts(Options{DirectorAddr: addr})}
			defer s.Logout() //nolint:errcheck
			for _, u := range []string{"a@example.com", "b@example.com"} {
				if got, err := s.directorLookup(u, ""); err != nil || got != "10.0.0.9:24" {
					t.Fatalf("lookup %s = %q, %v", u, got, err)
				}
			}
			if n := accepted.Load(); n != c.wantDial {
				t.Errorf("director connections = %d, want %d", n, c.wantDial)
			}
		})
	}
}
