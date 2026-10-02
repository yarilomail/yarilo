// Package linetest checks that a line-oriented server bounds the line it reads.
package linetest

import (
	"crypto/tls"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// ExpectCutOff dials addr, sends prefix and then 8 MiB with no line end, and
// fails unless the server closes the connection: an unbounded reader would
// keep holding the bytes and wait for an LF that never comes.
func ExpectCutOff(t *testing.T, addr string, tlsCfg *tls.Config, prefix string) {
	t.Helper()
	var conn net.Conn
	var err error
	if tlsCfg != nil {
		conn, err = tls.Dial("tcp", addr, tlsCfg)
	} else {
		conn, err = net.Dial("tcp", addr)
	}
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close() //nolint:errcheck
	go func() {
		_, _ = conn.Write([]byte(prefix))
		_, _ = conn.Write([]byte(strings.Repeat("A", 8<<20)))
	}()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.Copy(io.Discard, conn); err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatalf("%s: the server still holds the connection after 8 MiB with no line end", addr)
		}
	}
}
