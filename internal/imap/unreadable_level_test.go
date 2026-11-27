package imap_test

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
)

// syncBuffer is a log sink the server's goroutines write to while the test
// reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// captureLog sends every level to a buffer for the test's length.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// levelOf is the level of the first line carrying msg, or "" when none does.
func levelOf(log, msg string) string {
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, msg) {
			if i := strings.Index(l, "level="); i >= 0 {
				return strings.Fields(l[i+len("level="):])[0]
			}
		}
	}
	return ""
}

// A message expunged under a read is not an error, as the reference has it
// for FETCH, SEARCH and SORT: counted, and a Debug line. A message that is
// there and cannot be read stays a WARN, and is counted too.
func TestAnExpungedMessageIsDebugAndAnUnreadableOneWarn(t *testing.T) {
	damage := map[string]func(t *testing.T, path string){
		"gone": func(t *testing.T, path string) { mustRemove(t, path) },
		"unreadable": func(t *testing.T, path string) {
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
		},
	}
	want := map[string]string{"gone": "DEBUG", "unreadable": "WARN"}
	for _, cmd := range []struct {
		name, line, counter, msg string
	}{
		{"fetch", `FETCH 1 (ENVELOPE BODY.PEEK[TEXT])`, "fetch", "fetch answered without attributes it could not read"},
		{"search", `SEARCH TEXT alice`, "search", "search could not read some messages"},
		{"sort", `SORT (ARRIVAL) UTF-8 TEXT alice`, "sort", "could not read some messages; the answer is wrong about them"},
	} {
		for _, reason := range []string{"gone", "unreadable"} {
			t.Run(cmd.name+"/"+reason, func(t *testing.T) {
				root, addr := startEnvelopeCacheServer(t)
				c := dialRaw(t, addr)
				c.login()
				appendEnvelopeMsg(t, c)
				c.cmd(`SELECT INBOX`)
				damage[reason](t, storedMessagePath(t, root))
				log := captureLog(t)
				before := unreadableByReason(t, cmd.counter, reason)
				c.cmd(cmd.line)
				if got := unreadableByReason(t, cmd.counter, reason) - before; got < 1 {
					t.Fatalf("%s counted %v under reason=%s, want at least 1", cmd.name, got, reason)
				}
				if got := levelOf(log.String(), cmd.msg); got != want[reason] {
					t.Errorf("%s with a %s message logged at %q, want %s:\n%s", cmd.name, reason, got, want[reason], log.String())
				}
			})
		}
	}
}
