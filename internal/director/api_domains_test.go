package director

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/cluster/ring"
)

func domainMoveRequest(s *Server, domain, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/director/domains/"+domain+"/move", strings.NewReader(body))
	req.SetPathValue("domain", domain)
	rec := httptest.NewRecorder()
	s.apiDomainMove(rec, req)
	return rec
}

// The operator's move takes the rebalancer's path: the domain lands on the
// named backend, this director's own session is kicked, a replica is left to
// its owner and does not take the director down (#2187).
func TestADomainMoveByTheOperatorKicksOwnSessionsOnly(t *testing.T) {
	s := rebalanceServer(t, 0)
	from := place(s, "u1@one.test")
	to := "10.0.0.1"
	if from == to {
		to = "10.0.0.2"
	}
	own := &captureConn{}
	s.handleSessionOpen(&client{conn: own}, []string{"SESSION-OPEN", "own-1", "u1@one.test", from, "imap"})
	s.applyRemoteSessionOpen([]string{"peer-1", "u2@one.test", from, "imap"}, "10.9.9.8:9090#1")

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("the move panicked: %v", r)
		}
	}()
	rec := domainMoveRequest(s, "one.test", `{"backend":"`+to+`"}`)

	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"moved":"true"`) {
		t.Fatalf("status %d %s, want a move", rec.Code, rec.Body.String())
	}
	if got := hostIP(s.domainDir.Get("one.test").Host); got != to {
		t.Errorf("the domain is on %s, want %s", got, to)
	}
	if !strings.Contains(string(own.written), "USER-KICKED\tu1@one.test") {
		t.Errorf("the director's own session was not kicked: %q", own.written)
	}
}

func TestADomainMoveIsRefusedWhereItCannotLand(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(*Server)
		domain string
		body   string
		want   int
	}{
		{"assignment is not by domain", func(s *Server) { s.opts.AssignmentPolicy = policyHash }, "one.test", `{"backend":"10.0.0.2"}`, 409},
		{"no backend named", nil, "one.test", `{}`, 400},
		{"backend not in the ring", nil, "one.test", `{"backend":"10.0.0.9"}`, 404},
		{"backend down", func(s *Server) {
			s.ring.AddBackend(&ring.Backend{IP: "10.0.0.3", Port: 10143, Tag: "a", Up: false, Vhosts: 100})
		}, "one.test", `{"backend":"10.0.0.3"}`, 404},
		{"domain not placed", nil, "none.test", `{"backend":"10.0.0.2"}`, 404},
		{"backend in another tag", func(s *Server) {
			s.ring.AddBackend(&ring.Backend{IP: "10.0.0.4", Port: 10143, Tag: "b", Up: true, Vhosts: 100})
		}, "one.test", `{"backend":"10.0.0.4"}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := rebalanceServer(t, 0)
			place(s, "u1@one.test")
			if tc.setup != nil {
				tc.setup(s)
			}
			if rec := domainMoveRequest(s, tc.domain, tc.body); rec.Code != tc.want {
				t.Errorf("status %d (%s), want %d", rec.Code, strings.TrimSpace(rec.Body.String()), tc.want)
			}
		})
	}
}

// A move to where the domain already is changes nothing and kicks no one.
func TestADomainMoveToItsOwnBackendIsANoOp(t *testing.T) {
	s := rebalanceServer(t, 0)
	at := place(s, "u1@one.test")
	own := &captureConn{}
	s.handleSessionOpen(&client{conn: own}, []string{"SESSION-OPEN", "own-1", "u1@one.test", at, "imap"})

	rec := domainMoveRequest(s, "one.test", `{"backend":"`+at+`"}`)

	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"moved":"false"`) {
		t.Fatalf("status %d %s, want a no-op", rec.Code, rec.Body.String())
	}
	if strings.Contains(string(own.written), "USER-KICKED") {
		t.Errorf("a no-op move kicked: %q", own.written)
	}
}
