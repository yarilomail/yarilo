package login

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// The login log names why a session was kicked, as the director said it, not
// one fixed "admin or move" for every cause (#2263).
func TestAKickedSessionLogsItsReason(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s := &Server{opts: testOpts(Options{Protocol: ProtocolIMAP}), sessions: make(map[string][]*liveSession)}
	sess, client, done := proxiedPair(t, "s1", "u@example.com")
	s.sessions["u@example.com"] = []*liveSession{sess}
	_ = client

	s.kickUser("u@example.com", "backend-removed")
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the kicked session did not end")
	}
	if want := `"reason":"kicked: backend-removed"`; !strings.Contains(buf.String(), want) {
		t.Errorf("the kick logged %q, want %s", buf.String(), want)
	}
}
