package managesieve

import (
	"net"
	"testing"

	"github.com/yarilomail/yarilo/internal/sieve"
)

// The server's limits reach every session it opens.
func TestSessionsTakeTheServerLimits(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	sess := New(Options{MaxScriptSize: 100, MaxScripts: 3, MaxStorage: 500}).newSession(a, "u", t.TempDir(), "sid")
	if sess.maxSize != 100 || sess.maxScripts != 3 || sess.maxStorage != 500 {
		t.Errorf("session limits = size %d, count %d, storage %d; want 100, 3, 500", sess.maxSize, sess.maxScripts, sess.maxStorage)
	}
	if _, ok := sess.store.(*sieve.IndexedStore); !ok {
		t.Errorf("session store is %T, want the indexed store: a ManageSieve change must move the JMAP state", sess.store)
	}
}
