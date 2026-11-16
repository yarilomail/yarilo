package loginproto

import (
	"io"
	"net"
	"testing"
)

func TestNewPreambleConnReadsThrough(t *testing.T) {
	c, s := net.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() }) //nolint:errcheck
	pc := NewPreambleConn(s, "u@x", "/home/u", "sid")
	go func() { _, _ = c.Write([]byte("STAT\r\n")) }()
	buf := make([]byte, 6)
	if _, err := io.ReadFull(pc, buf); err != nil || string(buf) != "STAT\r\n" {
		t.Fatalf("read %q, %v", buf, err)
	}
	if pc.Username != "u@x" || pc.SessionID != "sid" || pc.RemoteAddr() == nil {
		t.Fatalf("got %+v", pc)
	}
}
