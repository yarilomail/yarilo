package director

import (
	"fmt"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/cluster/ring"
)

func rebalanceServer(t *testing.T, pct int) *Server {
	t.Helper()
	s := NewWithOptions(Options{
		AssignmentPolicy:       policyDomain,
		AntiEntropyInterval:    -1,
		DomainRebalancePercent: pct,
		LocalIP:                "10.9.9.9",
		LocalPort:              9090,
	})
	for _, ip := range []string{"10.0.0.1", "10.0.0.2"} {
		s.ring.AddBackend(&ring.Backend{IP: ip, Port: 10143, Tag: "a", Up: true, Vhosts: 100})
	}
	return s
}

// loadDomain places a domain and gives it n sessions of its own users on the
// backend it landed on: a session with no username belongs to no domain, and a
// rebalance judging those would be judging nothing.
func loadDomain(s *Server, domain string, n int) string {
	ip := place(s, "u1@"+domain)
	for i := 0; i < n; i++ {
		addUserSess(s, fmt.Sprintf("%s-%d", domain, i), ip, fmt.Sprintf("u%d@%s", i, domain))
	}
	return ip
}

// addUserSess opens a session the way the login proxy does, so the record
// carries both the username the director matches domains by and a client a
// kick can be written to.
func addUserSess(s *Server, id, backend, user string) {
	s.handleSessionOpen(&client{conn: &captureConn{}}, []string{"SESSION-OPEN", id, user, backend, "imap"})
}

// A spread under the threshold is left alone: a rebalance that fires on any
// difference disconnects mailboxes for nothing.
func TestASpreadUnderTheThresholdMovesNothing(t *testing.T) {
	s := rebalanceServer(t, 50)
	a := loadDomain(s, "one.test", 10)
	loadDomain(s, "two.test", 8)
	before := s.domainDir.Get("one.test").Host

	s.rebalanceDomains()

	if got := s.domainDir.Get("one.test").Host; got != before {
		t.Errorf("a domain moved from %s to %s on a spread below the threshold", before, got)
	}
	_ = a
}

// Above it, one domain moves down -- and only one, so the next pass judges a
// load that has already changed.
func TestAnImbalanceMovesOneDomain(t *testing.T) {
	s := rebalanceServer(t, 20)
	hot := loadDomain(s, "one.test", 30)
	// A second, small domain on the same backend: it is the one that moves.
	place(s, "u1@two.test")
	s.domainDir.Set("two.test", fmt.Sprintf("%s:10143", hot))

	s.rebalanceDomains()

	moved := s.domainDir.Get("two.test").Host
	stayed := s.domainDir.Get("one.test").Host
	if hostIP(moved) == hot {
		t.Errorf("the quiet domain stayed on the busy backend %s", hot)
	}
	if hostIP(stayed) != hot {
		t.Errorf("the loud domain moved too: %s", stayed)
	}
}

// The hysteresis: a domain just moved is left alone, or two backends either
// side of the threshold trade it back and forth for ever.
func TestAMovedDomainIsNotMovedAgainWithinTheCooldown(t *testing.T) {
	s := rebalanceServer(t, 20)
	s.opts.DomainRebalanceCooldown = time.Hour
	hot := loadDomain(s, "one.test", 30)
	place(s, "u1@two.test")
	s.domainDir.Set("two.test", fmt.Sprintf("%s:10143", hot))

	s.rebalanceDomains()
	after := s.domainDir.Get("two.test").Host

	// Load the new home so the imbalance now points the other way.
	for i := 0; i < 60; i++ {
		addUserSess(s, fmt.Sprintf("x%d", i), hostIP(after), fmt.Sprintf("x%d@three.test", i))
	}
	s.rebalanceDomains()

	if got := s.domainDir.Get("two.test").Host; got != after {
		t.Errorf("the domain moved again inside the cooldown: %s -> %s", after, got)
	}
}

// A move ends the sessions the domain left behind, so no client keeps writing
// to the backend it no longer belongs to.
func TestAMoveKicksTheSessionsItLeftBehind(t *testing.T) {
	s := rebalanceServer(t, 20)
	hot := loadDomain(s, "one.test", 30)
	place(s, "u1@two.test")
	s.domainDir.Set("two.test", fmt.Sprintf("%s:10143", hot))
	addUserSess(s, "two-live", hot, "u1@two.test")

	s.rebalanceDomains()

	s.sessRecMu.RLock()
	_, still := s.sessById["two-live"]
	s.sessRecMu.RUnlock()
	if still {
		t.Error("a session of the moved domain is still recorded on the backend it left")
	}
}
