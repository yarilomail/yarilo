package mailops

import (
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/userstate/threads"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/locks/lockstest"
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
// conversation is announced on the account's threads; a copy is not.
func TestRecordingAThreadEmits(t *testing.T) {
	rec := &lockstest.Recorder{}
	user := &mailbox.UserInfo{Username: "u@example.com", Home: t.TempDir()}
	o := &Ops{cfg: Config{Threads: threads.NewRecorder(nil), Protocol: "test", Locker: rec}, user: user}
	raw := []byte("Message-ID: <a@x>\r\nSubject: s\r\n\r\n")
	steps := []struct {
		name  string
		guid  byte
		emits bool
	}{
		{"new message", 1, true},
		{"copy of it", 1, false},
		{"another message", 2, true},
	}
	key := locks.ThreadsKey(user.Username)
	for _, st := range steps {
		before := rec.Count(key, locks.EventThreads)
		o.recordThread([16]byte{st.guid}, raw)
		if got := rec.Count(key, locks.EventThreads) - before; got != map[bool]int{true: 1}[st.emits] {
			t.Errorf("%s emitted %d, want emits=%v", st.name, got, st.emits)
		}
	}
}
