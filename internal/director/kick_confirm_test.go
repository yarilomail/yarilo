package director

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/cluster/ring"
)

// A user moved off a backend is held at LOOKUP until the login closes the
// kicked session; that close must release the hold after the confirm grace, not
// at user_kill_timeout, which outlasts the login's own wait for the hold.
func TestAMovedUsersHoldEndsWithTheLoginsClose(t *testing.T) {
	const (
		user  = "u1@one.test"
		old   = "10.0.0.1"
		moved = "10.0.0.2"
		grace = 50 * time.Millisecond
	)
	cases := []struct {
		name string
		move func(s *Server, hash uint32)
	}{
		{"domain move", func(s *Server, _ uint32) {
			s.kickDomainSessions("one.test", old+":10143")
		}},
		{"user assignment from a peer", func(s *Server, hash uint32) {
			s.applyKilling(hash, 10*time.Second)
			s.membership.applyEnvelope("USER-ASSIGN",
				[]string{fmt.Sprint(hash), moved + ":10143", "99", "10.0.0.1:9102"}, "10.0.0.99:9102", 1)
		}},
		{"user snapshot from a peer", func(s *Server, hash uint32) {
			s.applyKilling(hash, 10*time.Second)
			s.membership.applyUserLine([]string{"USER", fmt.Sprint(hash), moved + ":10143", "99", "10.0.0.1:9102", "0"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewWithOptions(testOptions(Options{
				AssignmentPolicy:     policyDomain,
				UserKillConfirmGrace: grace,
				UserKillTimeout:      10 * time.Second,
				LocalIP:              "10.9.9.9",
				LocalPort:            9090,
			}))
			for _, ip := range []string{old, moved} {
				s.ring.AddBackend(&ring.Backend{IP: ip, Port: 10143, Tag: "a", Up: true, Vhosts: 100})
			}
			hash := HashUsername(user, s.hf)
			s.userDir.Set(user, old+":10143", false)
			login := &client{conn: &captureConn{}}
			s.handleSessionOpen(login, []string{"SESSION-OPEN", "s1", user, old, "imap"})

			tc.move(s, hash)

			if !strings.Contains(string(login.conn.(*captureConn).written), "USER-KICKED\t"+user) {
				t.Fatalf("the session on the backend left behind was not kicked: %q", login.conn.(*captureConn).written)
			}
			if !s.isKilling(hash) {
				t.Fatal("the user is not held while the kicked session is still open")
			}
			// Still counted on the old backend until the login closes it.
			if n := s.backendSessionCounts()[old]; n != 1 {
				t.Errorf("backend_sessions for the old backend is %d before the close, want 1", n)
			}

			s.handleSessionClose(login, []string{"SESSION-CLOSE", "s1"})
			if n := s.backendSessionCounts()[old]; n != 0 {
				t.Errorf("backend_sessions for the old backend is %d after the close, want 0", n)
			}
			time.Sleep(grace + 30*time.Millisecond)
			s.sweepKills(grace)
			if s.isKilling(hash) {
				t.Error("the hold outlived the confirm grace after the login closed the session")
			}
		})
	}
}
