package mailops

import (
	"strings"
	"testing"
)

// Threading reads headers only, so a save keeps the header block and nothing
// of the body, written in pieces small enough to split the empty line.
func TestHeaderCaptureKeepsOnlyTheHeaderBlock(t *testing.T) {
	body := strings.Repeat("x", 1<<20)
	cases := []struct {
		name, msg, want string
	}{
		{"crlf", "Message-ID: <a@x>\r\nSubject: s\r\n\r\n" + body, "Message-ID: <a@x>\r\nSubject: s\r\n\r\n"},
		{"lf", "Message-ID: <a@x>\nSubject: s\n\n" + body, "Message-ID: <a@x>\nSubject: s\n\n"},
		{"no empty line", strings.Repeat("H: v\r\n", maxThreadHeader), strings.Repeat("H: v\r\n", maxThreadHeader)[:maxThreadHeader]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var h headerCapture
			for rest := tc.msg; rest != ""; {
				n := min(7, len(rest))
				if w, err := h.Write([]byte(rest[:n])); w != n || err != nil {
					t.Fatalf("write %d: %d %v", n, w, err)
				}
				rest = rest[n:]
			}
			if string(h.buf) != tc.want {
				t.Errorf("kept %d bytes, want %d", len(h.buf), len(tc.want))
			}
		})
	}
}
