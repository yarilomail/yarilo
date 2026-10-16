package director

import (
	"strings"
	"testing"
)

// An evacuation and a domain move each say why they kicked, so the login log
// names the cause rather than "admin or move" for all (#2263).
func TestEvacuationAndDomainMoveKickWithTheirReason(t *testing.T) {
	t.Run("flush", func(t *testing.T) {
		s, ip, _ := evacHarness(t, 1, 0)
		watcher := &captureConn{}
		s.addClient(&client{conn: watcher})
		s.startEvacuation(ip, 0)
		if want := "USER-KICKED\tu0@d.test\treason=flush"; !strings.Contains(string(watcher.written), want) {
			t.Errorf("the evacuation's kick is %q, want a line %q", watcher.written, want)
		}
	})
	t.Run("domain-move", func(t *testing.T) {
		s := domainServer(t, gone, alive)
		login := &captureConn{}
		s.sessRecMu.Lock()
		s.sessById["s1"] = &sessionRec{id: "s1", user: "u1@one.test", backend: gone, proto: "imap", cl: &client{conn: login}}
		s.sessByBE[gone] = map[string]bool{"s1": true}
		s.sessRecMu.Unlock()
		s.kickDomainSessions("one.test", gone+":10143")
		if want := "USER-KICKED\tu1@one.test\treason=domain-move"; !strings.Contains(string(login.written), want) {
			t.Errorf("the domain move's kick is %q, want a line %q", login.written, want)
		}
	})
}
