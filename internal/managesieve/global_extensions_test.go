package managesieve

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/sieve"
)

// A user's session under a global-only fileinto: CAPABILITY leaves it out and
// PUTSCRIPT / CHECKSCRIPT refuse a script that requires it (#2290).
func TestSessionLeavesGlobalExtensionsOut(t *testing.T) {
	set := sieve.ExtensionSet{List: []string{"fileinto", "envelope"}, Deny: []string{"fileinto"}}
	const script = `require "fileinto"; fileinto "X";`
	for _, c := range []struct {
		send   string
		wantOK bool
		check  func(lines []string) string
	}{
		{"CAPABILITY\r\n", true, func(lines []string) string {
			for _, l := range lines {
				if strings.HasPrefix(l, `"SIEVE"`) && (strings.Contains(l, "fileinto") || !strings.Contains(l, "envelope")) {
					return "SIEVE line " + l + ", want envelope without fileinto"
				}
			}
			return ""
		}},
		{fmt.Sprintf("PUTSCRIPT \"s\" {%d+}\r\n%s", len(script), script), false, nil},
		{fmt.Sprintf("CHECKSCRIPT {%d+}\r\n%s", len(script), script), false, nil},
	} {
		client, server := net.Pipe()
		sess := &session{conn: server, r: bufio.NewReader(server), w: bufio.NewWriter(server),
			username: "u1@example.com", homeDir: t.TempDir(), store: newTestStore(),
			maxSize: 65536, maxLine: 65536, allowedExtensions: set}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		go sess.serve(ctx)
		tc := &testClient{t: t, conn: client, r: bufio.NewReader(client)}
		tc.readUntilResult()
		tc.send(c.send)
		lines, ok := tc.readUntilResult()
		if ok != c.wantOK {
			t.Errorf("%q: ok=%v %v, want ok=%v", c.send, ok, lines, c.wantOK)
		}
		if c.check != nil {
			if msg := c.check(lines); msg != "" {
				t.Error(msg)
			}
		}
		tc.close()
		cancel()
	}
}
