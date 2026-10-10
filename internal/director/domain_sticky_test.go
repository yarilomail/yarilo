package director

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/yarilomail/yarilo/internal/cluster/ring"
)

// stickyHits is how many LOOKUPs the sticky entry answered.
func stickyHits(t *testing.T) uint64 {
	t.Helper()
	m := &dto.Metric{}
	if err := lookupSeconds.WithLabelValues("sticky").(prometheus.Metric).Write(m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleCount()
}

func lookup(s *Server, user string) string {
	conn := &captureConn{}
	s.handleLookup(&client{conn: conn}, []string{"LOOKUP", "7", user, "a", "imap"})
	return string(conn.written)
}

// A move rewrites the placement, not the entries: the next LOOKUP of a user
// pinned before it must follow the domain and rewrite the entry (#2193).
func TestALookupAfterADomainMoveFollowsTheDomain(t *testing.T) {
	s := domainServer(t, "10.0.0.1", "10.0.0.2")
	user := "u1@one.test"
	if got := lookup(s, user); !strings.Contains(got, "HOST\t7\t10.0.0.1\t") {
		t.Fatalf("first login: %q, want 10.0.0.1 (the less loaded, lower IP)", got)
	}
	s.moveDomain("one.test", "10.0.0.1:10143", "10.0.0.2:10143")

	if got := lookup(s, user); !strings.Contains(got, "HOST\t7\t10.0.0.2\t") {
		t.Fatalf("after the move the sticky entry still routes the user: %q, want 10.0.0.2", got)
	}
	if e := s.userDir.Get(user); e == nil || e.Host != "10.0.0.2:10143" {
		t.Errorf("entry after the move's LOOKUP = %+v, want 10.0.0.2:10143", e)
	}
}

// An entry that names the placement stays sticky: no reassignment, so no
// new assignment for every login of a placed domain.
func TestAnEntryMatchingThePlacementStaysSticky(t *testing.T) {
	s := domainServer(t, "10.0.0.1", "10.0.0.2")
	user := "u1@one.test"
	lookup(s, user)
	before := stickyHits(t)
	if got := lookup(s, user); !strings.Contains(got, "HOST\t7\t10.0.0.1\t") {
		t.Fatalf("second login: %q, want the sticky 10.0.0.1", got)
	}
	if n := stickyHits(t) - before; n != 1 {
		t.Errorf("the entry answered %d of one LOOKUP, want 1: a matching entry must not be reassigned", n)
	}
}

// Under hash the entry is all there is: a placement, even a stale one, is not
// consulted.
func TestUnderHashTheEntryIsNotCheckedAgainstAPlacement(t *testing.T) {
	s := NewWithOptions(testOptions(Options{AssignmentPolicy: policyHash}))
	for _, ip := range []string{"10.0.0.1", "10.0.0.2"} {
		s.ring.AddBackend(&ring.Backend{IP: ip, Port: 10143, Tag: "a", Up: true, Vhosts: 100})
	}
	user := "u1@one.test"
	// Pinned off its hash backend, the placement on it: only the entry
	// answers with the pin.
	hashed := s.ring.LookupBackendByTag(user, "a").IP
	pin := "10.0.0.1"
	if hashed == pin {
		pin = "10.0.0.2"
	}
	s.userDir.Set(user, pin+":10143", false)
	s.domainDir.Set("one.test", hashed+":10143")
	if got := lookup(s, user); !strings.Contains(got, "HOST\t7\t"+pin+"\t") {
		t.Errorf("hash policy: %q, want the sticky %s", got, pin)
	}
}

func TestSameHostComparesParts(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"10.0.0.1:10143", "10.0.0.1:10143", true},
		{"10.0.0.1:10143", "10.0.0.1:10144", false},
		{"10.0.0.1:10143", "10.0.0.2:10143", false},
		{"[fd00::1]:10143", "[fd00:0::1]:10143", true},
		{"a.test:10143", "b.test:10143", false},
	} {
		if got := sameHost(c.a, c.b); got != c.want {
			t.Errorf("sameHost(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
