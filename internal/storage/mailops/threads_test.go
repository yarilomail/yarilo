package mailops

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/userstate/threads"
	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/locks/lockstest"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
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

// A save that crosses the tail size returns while the merge it triggered is
// held, and a second due merge while one runs is left to the next save
// (#2651).
func TestASaveDoesNotWaitForTheMerge(t *testing.T) {
	started := make(chan struct{}, 4)
	release, finished := make(chan struct{}), make(chan struct{}, 4)
	mergeSeam = func(done bool) {
		if done {
			// "Finished" is the state a save after it sees: no merge running.
			if merging.Load() {
				t.Error("the merge said it finished while still marked as running")
			}
			finished <- struct{}{}
			return
		}
		started <- struct{}{}
		<-release
	}
	t.Cleanup(func() { mergeSeam = nil })
	ops := func() *Ops {
		user := &mailbox.UserInfo{Username: "u@example.com", Home: t.TempDir()}
		return &Ops{cfg: Config{Threads: threads.NewRecorder(nil, threads.WithTailSize(1)), Protocol: "test"}, user: user}
	}
	a, b := ops(), ops()
	raw := []byte("Message-ID: <a@x>\r\nSubject: s\r\n\r\n")
	returned := make(chan struct{})
	go func() {
		a.recordThread([16]byte{1}, raw)
		close(returned)
	}()
	<-started // the merge runs, held by the seam until the save is back
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the save waited for the merge it triggered")
	}
	b.recordThread([16]byte{2}, raw)
	close(release)
	<-finished
	select {
	case <-started:
		t.Error("a second merge ran while one was in flight")
	default:
	}
	b.recordThread([16]byte{3}, raw)
	<-started
	<-finished
	if _, err := os.Stat(threads.PathFor(b.user) + threads.BaseSuffix); err != nil {
		t.Errorf("the next save did not merge what was left: %v", err)
	}
}
