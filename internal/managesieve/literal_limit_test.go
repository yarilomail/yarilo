package managesieve

import (
	"strings"
	"testing"
	"time"
)

// A literal size is checked before anything is allocated or read.
func TestALiteralOverItsLimitIsRefusedUnread(t *testing.T) {
	for _, tc := range []struct {
		name, send string
		want       string
		closed     bool
	}{
		{"negative", "PUTSCRIPT \"a\" {-1+}\r\n", "NO", false},
		{"name over the line limit, sync", "GETSCRIPT {65537}\r\n", `NO "Literal size too large."`, false},
		{"name over the line limit", "GETSCRIPT {65537+}\r\n", `NO "Literal size too large."`, true},
		{"script over its limit", "PUTSCRIPT \"a\" {65537+}\r\n", `NO (QUOTA/MAXSCRIPTSIZE) "Script too large."`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := runSession(t, newTestStore(), t.TempDir())
			c.conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
			c.send(tc.send)
			lines, _ := c.readUntilResult()
			if got := lines[len(lines)-1]; !strings.HasPrefix(got, tc.want) {
				t.Fatalf("reply = %q, want %q", got, tc.want)
			}
			if tc.closed {
				lines, _ := c.readUntilResult()
				if !strings.HasPrefix(lines[len(lines)-1], "BYE") {
					t.Fatalf("after the refusal got %q, want BYE", lines)
				}
				return
			}
			c.send("NOOP\r\n")
			if lines, ok := c.readUntilResult(); !ok {
				t.Fatalf("NOOP after the refusal = %q, want OK", lines)
			}
		})
	}
}

// A literal at the limit is read in full.
func TestALiteralAtItsLimitIsRead(t *testing.T) {
	c := runSession(t, newTestStore(), t.TempDir())
	c.conn.SetReadDeadline(time.Now().Add(2 * time.Second)) //nolint:errcheck
	// net.Pipe has no buffer: the reply would wait on the rest of this write.
	go c.conn.Write([]byte("GETSCRIPT {65536+}\r\n" + strings.Repeat("a", 65536) + "\r\n")) //nolint:errcheck
	lines, _ := c.readUntilResult()
	if got := lines[len(lines)-1]; strings.Contains(got, "too large") {
		t.Fatalf("reply = %q, want the script looked up", got)
	}
	c.send("NOOP\r\n")
	if lines, ok := c.readUntilResult(); !ok {
		t.Fatalf("NOOP = %q, want OK", lines)
	}
}
