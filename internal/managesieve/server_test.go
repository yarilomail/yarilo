package managesieve

import (
	"net"
	"testing"
)

// The server's limits reach every session it opens.
func TestSessionsTakeTheServerLimits(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	sess := New(Options{MaxScriptSize: 100, MaxScripts: 3}).newSession(a, "u", t.TempDir(), "sid")
	if sess.maxSize != 100 || sess.maxScripts != 3 {
		t.Errorf("session limits = size %d, count %d; want 100 and 3", sess.maxSize, sess.maxScripts)
	}
}
