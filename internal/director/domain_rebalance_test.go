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

// Above it, one domain moves down -- the one whose move narrows the spread
// most, and only one, so the next pass judges a load that has already changed.
func TestAnImbalanceMovesTheDomainThatNarrowsItMost(t *testing.T) {
	s := rebalanceServer(t, 20)
	hot := loadDomain(s, "one.test", 30)
	// A second domain on the same backend. Moving it leaves 18 against 12;
	// moving the first would leave 12 against 30, which is worse than doing
	// nothing about it.
	onHot(s, "two.test", hot, 12)

	s.rebalanceDomains()

	if got := hostIP(s.domainDir.Get("two.test").Host); got == hot {
		t.Errorf("the domain that would settle the spread stayed on %s", hot)
	}
	if got := hostIP(s.domainDir.Get("one.test").Host); got != hot {
		t.Errorf("the bigger domain moved too: %s", got)
	}
}

// One big domain alone on a backend is both the quietest and the loudest
// there: moving it turns the imbalance over instead of settling it, and after
// the cooldown it would come straight back. Nothing moves (#1943).
func TestADomainThatWouldOnlyFlipTheImbalanceStays(t *testing.T) {
	s := rebalanceServer(t, 20)
	big := loadDomain(s, "one.test", 40)
	other := "10.0.0.2"
	if big == other {
		other = "10.0.0.1"
	}
	onHost(s, "two.test", other, 4)

	s.rebalanceDomains()

	if got := hostIP(s.domainDir.Get("one.test").Host); got != big {
		t.Errorf("the only domain on the busy backend moved to %s; the spread would only have flipped", got)
	}
}

// onHot places a domain on a named backend and gives it n sessions there.
func onHot(s *Server, domain, ip string, n int) { onHost(s, domain, ip, n) }

// reconnect gives a domain sessions on the backend it now belongs to, as its
// clients do after a move ended the ones they had.
func reconnect(s *Server, domain, ip string, n int) {
	for i := 0; i < n; i++ {
		addUserSess(s, fmt.Sprintf("%s-re-%d", domain, i), ip, fmt.Sprintf("u%d@%s", i, domain))
	}
}

func onHost(s *Server, domain, ip string, n int) {
	s.domainDir.Set(domain, fmt.Sprintf("%s:10143", ip))
	for i := 0; i < n; i++ {
		addUserSess(s, fmt.Sprintf("%s-%d", domain, i), ip, fmt.Sprintf("u%d@%s", i, domain))
	}
}

// The hysteresis: a domain just moved is left alone even when moving it back
// would settle the spread, or two backends either side of the line trade it
// for ever (#1943).
func TestAMovedDomainIsNotMovedAgainWithinTheCooldown(t *testing.T) {
	s := rebalanceServer(t, 20)
	s.opts.DomainRebalanceCooldown = time.Hour
	hot := loadDomain(s, "one.test", 30)
	onHot(s, "two.test", hot, 12)

	s.rebalanceDomains()
	after := hostIP(s.domainDir.Get("two.test").Host)
	if after == hot {
		t.Fatalf("the first pass moved nothing, so there is no cooldown to test")
	}

	// The moved domain's clients reconnect on their new backend, and a big
	// domain lands there too: that backend is now the busy one, and moving the
	// small one back is the only move that would narrow the spread -- moving
	// the big one would overshoot.
	reconnect(s, "two.test", after, 12)
	onHost(s, "big.test", after, 40)

	s.rebalanceDomains()

	if got := hostIP(s.domainDir.Get("two.test").Host); got != after {
		t.Errorf("the domain moved again inside the cooldown: %s -> %s", after, got)
	}
}

// A move ends the sessions the domain left behind, so no client keeps writing
// to the backend it no longer belongs to.
func TestAMoveKicksTheSessionsItLeftBehind(t *testing.T) {
	s := rebalanceServer(t, 20)
	hot := loadDomain(s, "one.test", 30)
	onHot(s, "two.test", hot, 12)

	s.rebalanceDomains()

	s.sessRecMu.RLock()
	_, still := s.sessById["two.test-0"]
	s.sessRecMu.RUnlock()
	if still {
		t.Error("a session of the moved domain is still recorded on the backend it left")
	}
}

// A probe for the row above: without the cooldown the same setup does move the
// domain back, so the row is testing the hysteresis and not the spread rule.
func TestTheCooldownIsWhatHoldsTheMoveBack(t *testing.T) {
	s := rebalanceServer(t, 20)
	s.opts.DomainRebalanceCooldown = time.Hour
	hot := loadDomain(s, "one.test", 30)
	onHot(s, "two.test", hot, 12)
	s.rebalanceDomains()
	after := hostIP(s.domainDir.Get("two.test").Host)
	reconnect(s, "two.test", after, 12)
	onHost(s, "big.test", after, 40)

	// The cooldown is the only thing in the way: with it forgotten, the move
	// back happens.
	s.domainMoves.mu.Lock()
	delete(s.domainMoves.moved, "two.test")
	s.domainMoves.mu.Unlock()
	s.rebalanceDomains()

	if got := hostIP(s.domainDir.Get("two.test").Host); got == after {
		t.Errorf("the domain stayed on %s even with the cooldown forgotten: the row above proves nothing about hysteresis", after)
	}
}
