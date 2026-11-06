package managesieve

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// sieve_max_redirects in a session: MAXREDIRECTS is advertised above 0, and
// 0 refuses a script with redirect at PUTSCRIPT and CHECKSCRIPT (#2529).
func TestSessionRedirectLimit(t *testing.T) {
	const script = `redirect "a@example.org";`
	put := fmt.Sprintf("PUTSCRIPT \"s\" {%d+}\r\n%s\r\n", len(script), script)
	check := fmt.Sprintf("CHECKSCRIPT {%d+}\r\n%s\r\n", len(script), script)
	hasCap := func(want string) func([]string) string {
		return func(lines []string) string {
			for _, l := range lines {
				if strings.HasPrefix(l, `"MAXREDIRECTS"`) {
					if l != want {
						return "got " + l + ", want " + want
					}
					return ""
				}
			}
			if want != "" {
				return "no MAXREDIRECTS, want " + want
			}
			return ""
		}
	}
	cases := []struct {
		name   string
		max    int
		send   string
		wantOK bool
		check  func([]string) string
	}{
		{"capability at 32", 32, "CAPABILITY\r\n", true, hasCap(`"MAXREDIRECTS" "32"`)},
		{"capability at 0", 0, "CAPABILITY\r\n", true, hasCap("")},
		{"putscript at 0", 0, put, false, nil},
		{"checkscript at 0", 0, check, false, nil},
		{"putscript at 1", 1, put, true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client, server := net.Pipe()
			sess := &session{conn: server, r: bufio.NewReader(server), w: bufio.NewWriter(server),
				username: "u1@example.com", homeDir: t.TempDir(), store: newTestStore(),
				maxSize: 65536, maxLine: 65536, maxRedirects: c.max}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			go sess.serve(ctx)
			tc := &testClient{t: t, conn: client, r: bufio.NewReader(client)}
			defer tc.close()
			tc.readUntilResult()
			tc.send(c.send)
			lines, ok := tc.readUntilResult()
			if ok != c.wantOK {
				t.Fatalf("ok=%v %v, want ok=%v", ok, lines, c.wantOK)
			}
			if c.check != nil {
				if msg := c.check(lines); msg != "" {
					t.Error(msg)
				}
			}
		})
	}
}
