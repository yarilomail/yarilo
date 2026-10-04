package director

import (
	"fmt"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/cluster/ring"
)

func domainServer(t *testing.T, backends ...string) *Server {
	t.Helper()
	s := NewWithOptions(Options{
		AssignmentPolicy:    policyDomain,
		AntiEntropyInterval: -1,
		LocalIP:             "10.9.9.9",
		LocalPort:           9090,
	})
	for _, ip := range backends {
		s.ring.AddBackend(&ring.Backend{IP: ip, Port: 10143, Tag: "a", Up: true, Vhosts: 100})
	}
	return s
}

// place is one login: the policy picks, and the placement is recorded and
// credited exactly as assignAndPin does it.
func place(s *Server, user string) string {
	b := s.pickBackend(user, "a", "imap")
	if b == nil {
		return ""
	}
	addr := fmt.Sprintf("%s:%d", b.IP, b.Port)
	s.recordDomain(user, addr)
	return b.IP
}

// A first domain lands somewhere; the second, while the first backend carries
// the load, lands elsewhere (#1943).
func TestASecondDomainAvoidsTheLoadedBackend(t *testing.T) {
	s := domainServer(t, "10.0.0.1", "10.0.0.2")
	first := place(s, "u1@one.test")
	if first == "" {
		t.Fatal("the first domain was not placed at all")
	}
	for i := 0; i < 20; i++ {
		addSess(s, fmt.Sprintf("s%d", i), first, "imap")
	}
	if second := place(s, "u1@two.test"); second == first {
		t.Errorf("both domains landed on %s; the second must go where the connections are fewer", second)
	}
}

// Every user of one domain reaches one backend, which is what a shared mailbox
// needs (#1931).
func TestEveryUserOfADomainLandsOnOneBackend(t *testing.T) {
	s := domainServer(t, "10.0.0.1", "10.0.0.2", "10.0.0.3")
	want := place(s, "u1@one.test")
	for i := 2; i <= 50; i++ {
		if got := place(s, fmt.Sprintf("u%d@one.test", i)); got != want {
			t.Fatalf("user %d of the domain landed on %s, the first on %s", i, got, want)
		}
	}
}

// The burst the reference survives by crediting the host at assignment time:
// fifty first logins across two new domains must not all land on one backend,
// because no SESSION-OPEN has come back yet (#1931, #1943).
func TestABurstOfFirstLoginsSplitsAcrossBackends(t *testing.T) {
	s := domainServer(t, "10.0.0.1", "10.0.0.2")
	seen := map[string]int{}
	for i := 1; i <= 50; i++ {
		domain := "one.test"
		if i%2 == 0 {
			domain = "two.test"
		}
		seen[place(s, fmt.Sprintf("u%d@%s", i, domain))]++
	}
	if len(seen) < 2 {
		t.Errorf("fifty first logins across two domains used %d backend(s): %v", len(seen), seen)
	}
}

// A domain with no traffic for its expiry is forgotten, and placed afresh.
func TestAnIdleDomainIsPlacedAfresh(t *testing.T) {
	s := domainServer(t, "10.0.0.1", "10.0.0.2")
	s.domainDir.expire = 20 * time.Millisecond
	first := place(s, "u1@one.test")
	time.Sleep(40 * time.Millisecond)
	if e := s.domainDir.Get("one.test"); e != nil {
		t.Fatalf("the domain outlived its expiry: %v", e)
	}
	// Load the first backend so a fresh placement has a reason to differ.
	for i := 0; i < 20; i++ {
		addSess(s, fmt.Sprintf("s%d", i), first, "imap")
	}
	if again := place(s, "u1@one.test"); again == first {
		t.Errorf("the expired domain was placed on %s again; it was not forgotten", again)
	}
}

// A backend that left the ring does not hold its domains: the next login
// places them, one at a time, rather than a wave at the moment it went down.
func TestADomainOfAGoneBackendIsPlacedAfresh(t *testing.T) {
	s := domainServer(t, "10.0.0.1", "10.0.0.2")
	first := place(s, "u1@one.test")
	s.ring.RemoveBackend(first)
	again := place(s, "u2@one.test")
	if again == "" || again == first {
		t.Errorf("the domain was placed on %q after its backend left the ring", again)
	}
	if e := s.domainDir.Get("one.test"); e == nil || hostIP(e.Host) != again {
		t.Errorf("the fresh placement was not recorded: %v", e)
	}
}

// A placement that arrived from a peer survives a restart of the director that
// made it: the entry is the ring's, not one director's.
func TestAPeersPlacementIsTaken(t *testing.T) {
	s := domainServer(t, "10.0.0.1", "10.0.0.2")
	if from := s.domainDir.Merge("one.test", "10.0.0.2:10143", 7, "10.1.1.1:9090"); from != "" {
		t.Fatalf("a first sighting reported a move from %q", from)
	}
	if got := place(s, "u1@one.test"); got != "10.0.0.2" {
		t.Errorf("the domain was placed on %s, ignoring the peer's assignment", got)
	}
}

// The credit a placement takes is released once its traffic is counted, or the
// next domain is sent away from a backend that is not in fact busier.
func TestThePlacementCreditIsReleasedWhenTheSessionArrives(t *testing.T) {
	s := domainServer(t, "10.0.0.1", "10.0.0.2")
	ip := place(s, "u1@one.test")
	if n := s.domainDir.Pending(ip); n != 1 {
		t.Fatalf("pending on %s = %d, want 1: the placement credited nothing", ip, n)
	}
	s.domainDir.Seen("one.test")
	if n := s.domainDir.Pending(ip); n != 0 {
		t.Errorf("pending on %s = %d after the session arrived, want 0", ip, n)
	}
}
