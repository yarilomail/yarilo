package login

import (
	"bufio"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/lineio"
)

// countingConn counts what the server pulled from the client.
type countingConn struct {
	net.Conn
	n *atomic.Int64
}

func (c countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// A client sending 8 MiB with no line end is cut off near the bound in every
// pre-auth loop: the loop ends with an error and reads no more than that.
func TestAnEndlessLineEndsThePreAuthLoop(t *testing.T) {
	const slack = 64 << 10
	for _, tc := range []struct {
		name, prefix string
		run          func(c net.Conn, rd *bufio.Reader) error
	}{
		{"IMAP", "", func(c net.Conn, rd *bufio.Reader) error {
			_, _, _, err := extractIMAPPreamble(c, rd, nil, Options{}, relayContext{})
			return err
		}},
		{"POP3", "", func(c net.Conn, rd *bufio.Reader) error {
			_, _, _, err := extractPOP3Preamble(c, rd, nil, Options{}, relayContext{})
			return err
		}},
		{"SMTP", "", func(c net.Conn, rd *bufio.Reader) error {
			_, _, _, err := extractSubmissionPreamble(c, rd, nil, Options{}, relayContext{})
			return err
		}},
		{"ManageSieve atom", "", func(c net.Conn, rd *bufio.Reader) error {
			_, _, _, err := extractManageSievePreamble(c, rd, nil, Options{})
			return err
		}},
		{"ManageSieve quoted", `AUTHENTICATE "`, func(c net.Conn, rd *bufio.Reader) error {
			_, _, _, err := extractManageSievePreamble(c, rd, nil, Options{})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, cli := pipePair(t)
			var read atomic.Int64
			cc := countingConn{Conn: srv, n: &read}
			done := make(chan error, 1)
			go func() { done <- tc.run(cc, bufio.NewReader(cc)) }()
			go func() { _, _ = bufio.NewReader(cli).ReadString(0) }() // drain replies
			go func() {
				cli.Write([]byte(tc.prefix))                  //nolint:errcheck
				cli.Write([]byte(strings.Repeat("A", 8<<20))) //nolint:errcheck
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("the loop returned without an error")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the loop is still reading")
			}
			if n := read.Load(); n > lineio.MaxClient+slack {
				t.Fatalf("read %d bytes, want at most %d", n, lineio.MaxClient+slack)
			}
		})
	}
}
