package login

import (
	"bufio"
	"strings"
	"testing"
	"time"
)

// Before authentication a literal is bounded before it is read: a negative
// size is a bad argument, one past the bound ends the session.
func TestAManageSieveLiteralIsBoundedBeforeAuth(t *testing.T) {
	for _, tc := range []struct {
		name, send, want string
		ended            bool
	}{
		{"negative", "AUTHENTICATE \"PLAIN\" {-1+}\r\n", "NO", false},
		{"one past the bound", "AUTHENTICATE \"PLAIN\" {8193+}\r\n", `NO "Literal size too large."`, true},
		{"at the bound", "AUTHENTICATE \"PLAIN\" {8192+}\r\n" + strings.Repeat("A", 8192) + "\r\n", "NO (AUTHENTICATIONFAILED)", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, cli := pipePair(t)
			errCh := make(chan error, 1)
			go func() {
				_, _, _, err := extractManageSievePreamble(srv, bufio.NewReader(srv), nil, Options{})
				errCh <- err
			}()
			cli.SetReadDeadline(time.Now().Add(3 * time.Second)) //nolint:errcheck
			crd := bufio.NewReader(cli)
			readMSGreeting(t, crd)
			go cli.Write([]byte(tc.send)) //nolint:errcheck
			line, err := crd.ReadString('\n')
			if err != nil || !strings.HasPrefix(line, tc.want) {
				t.Fatalf("reply = %q, %v; want %q", line, err, tc.want)
			}
			if tc.ended {
				select {
				case err := <-errCh:
					if err == nil {
						t.Fatal("the session went on, want it ended")
					}
				case <-time.After(2 * time.Second):
					t.Fatal("the session is still reading, want it ended")
				}
				return
			}
			cli.Write([]byte("NOOP\r\n")) //nolint:errcheck
			if line, err := crd.ReadString('\n'); err != nil || !strings.HasPrefix(line, "OK") {
				t.Fatalf("NOOP = %q, %v; want OK", line, err)
			}
		})
	}
}
