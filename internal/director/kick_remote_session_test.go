package director

import (
	"strings"
	"testing"
)

// A moved domain whose sessions include a replica another director owns: the
// move kicks this director's own session and leaves the replica to its owner.
// A replica has no conn, and writing the kick to it took two of three
// directors down on the sandbox (#2187).
func TestAMoveKicksOwnSessionsAndSkipsReplicas(t *testing.T) {
	s := rebalanceServer(t, 20)
	hot := loadDomain(s, "one.test", 30)
	onHot(s, "two.test", hot, 12)
	s.applyRemoteSessionOpen([]string{"peer-1", "u90@two.test", hot, "imap"}, "10.9.9.8:9090#1")
	own := &captureConn{}
	s.handleSessionOpen(&client{conn: own}, []string{"SESSION-OPEN", "own-1", "u91@two.test", hot, "imap"})

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("the move panicked on a replica's kick: %v", r)
		}
	}()
	s.rebalanceDomains()

	if !strings.Contains(string(own.written), "USER-KICKED\tu91@two.test") {
		t.Errorf("the director's own session of the moved domain was not kicked: %q", own.written)
	}
}
