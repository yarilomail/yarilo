package mailops

import (
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/userstate/counters"
	"github.com/yarilomail/yarilo/internal/userstate/threads"
	"github.com/yarilomail/yarilo/pkg/mailbox"
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

// The sidecar is written after the folder's commit, so recording a new
// conversation moves Thread again; a copy, already threaded, does not.
func TestRecordingAThreadMovesThread(t *testing.T) {
	home := t.TempDir()
	user := &mailbox.UserInfo{Username: "u@example.com", Home: home}
	o := &Ops{cfg: Config{Threads: threads.NewRecorder(nil), Protocol: "test"}, user: user}
	raw := []byte("Message-ID: <a@x>\r\nSubject: s\r\n\r\n")
	steps := []struct {
		name  string
		guid  byte
		moves bool
	}{
		{"new message", 1, true},
		{"copy of it", 1, false},
		{"another message", 2, true},
	}
	for _, st := range steps {
		before, _ := counters.Read(counters.Path(home))
		o.recordThread([16]byte{st.guid}, raw)
		after, _ := counters.Read(counters.Path(home))
		got := after.ChangedAfter(before.Highest)
		if moved := len(got) == 1 && got[0] == counters.Thread; moved != st.moves || (!st.moves && len(got) != 0) {
			t.Errorf("%s moved %v, want Thread=%v", st.name, got, st.moves)
		}
	}
}
