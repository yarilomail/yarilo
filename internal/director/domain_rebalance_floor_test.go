package director

import "testing"

// Two sessions against none is a 100 % spread, and a move would cut a client
// off for a load that is not there: below the floor nothing moves. The same
// spread on a backend that carries the floor still moves a domain (#2494).
func TestASpreadOnAQuietBackendMovesNothing(t *testing.T) {
	quiet := rebalanceServer(t, 20)
	quiet.opts.DomainRebalanceMinSessions = 50
	hot := loadDomain(quiet, "one.test", 1)
	onHot(quiet, "two.test", hot, 1)

	quiet.rebalanceDomains()

	for _, d := range []string{"one.test", "two.test"} {
		if got := hostIP(quiet.domainDir.Get(d).Host); got != hot {
			t.Errorf("%s moved to %s on two sessions against none", d, got)
		}
	}

	busy := rebalanceServer(t, 20)
	busy.opts.DomainRebalanceMinSessions = 50
	hot = loadDomain(busy, "one.test", 40)
	onHot(busy, "two.test", hot, 12)

	busy.rebalanceDomains()

	if got := hostIP(busy.domainDir.Get("two.test").Host); got == hot {
		t.Errorf("52 sessions against none moved nothing; the floor held a real imbalance")
	}
}
