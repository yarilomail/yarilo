package jmap

import (
	"encoding/json"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/jmapcore"
)

// The session offers WebSocket tickets only when the login layer says it
// issues them, and every response's sessionState matches the session the
// same login layer serves, or a client would refetch it forever (#2479).
func TestTicketsAreOfferedOnlyWhereIssued(t *testing.T) {
	s := New(Options{
		Trust:            ResolveTrust(false, true, []*net.IPNet{mustCIDR(t, "192.0.2.0/24")}),
		Limits:           testLimits(),
		WebSocketTimeout: time.Minute,
		WebSocketTicket:  true,
	})
	for _, tc := range []struct {
		name    string
		header  bool
		offered bool
	}{{"announced", true, true}, {"not announced", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			r := identityRequest(testUser, trustedPeer)
			if tc.header {
				r.Header.Set(hdrTickets, "1")
			}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			var sess struct {
				Capabilities map[string]json.RawMessage `json:"capabilities"`
				State        string                     `json:"state"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &sess); err != nil {
				t.Fatalf("session: %v: %s", err, w.Body.String())
			}
			if _, ok := sess.Capabilities[jmapcore.CapWebSocketTicket]; ok != tc.offered {
				t.Errorf("ticket capability offered = %v, want %v", ok, tc.offered)
			}

			a := apiRequest(`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{},"c0"]]}`)
			a.Header.Set(hdrUser, testUser)
			if tc.header {
				a.Header.Set(hdrTickets, "1")
			}
			w = httptest.NewRecorder()
			s.Handler().ServeHTTP(w, a)
			var resp struct {
				SessionState string `json:"sessionState"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("api: %v: %s", err, w.Body.String())
			}
			if resp.SessionState != sess.State {
				t.Errorf("api sessionState %q, session state %q", resp.SessionState, sess.State)
			}
		})
	}
}
