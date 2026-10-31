package jmapcore

import "testing"

// The ticket is advertised with WebSocket at the session and at the account,
// where the client looks for both; off, neither changes.
func TestTheTicketIsAdvertisedWithTheWebSocket(t *testing.T) {
	for _, tc := range []struct {
		name    string
		lim     Limits
		session bool
	}{
		{"on", Limits{BaseURL: "https://mail.example", WebSocket: &WebSocketCapability{SupportsPush: true}, WebSocketTicket: true}, true},
		{"off", Limits{BaseURL: "https://mail.example", WebSocket: &WebSocketCapability{SupportsPush: true}}, false},
		{"no WebSocket", Limits{BaseURL: "https://mail.example", WebSocketTicket: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := BuildSession(tc.lim, "u1@example.com")
			acc := s.Accounts[AccountID("u1@example.com")].AccountCapabilities
			_, inSession := s.Capabilities[CapWebSocketTicket]
			_, wsAccount := acc[CapWebSocket]
			ticket, inAccount := acc[CapWebSocketTicket].(WebSocketTicketCapability)
			if inSession != tc.session || inAccount != tc.session || wsAccount != tc.session {
				t.Fatalf("session %v, account %v, websocket in account %v; want %v", inSession, inAccount, wsAccount, tc.session)
			}
			if tc.session && ticket.GenerationEndpoint != "https://mail.example"+WebSocketTicketPath {
				t.Errorf("generationEndpoint %q", ticket.GenerationEndpoint)
			}
		})
	}
	off := Limits{BaseURL: "https://mail.example", WebSocket: &WebSocketCapability{}}
	on := off
	on.WebSocketTicket = true
	if SessionState(on) == SessionState(off) {
		t.Error("the session state does not move with the ticket")
	}
}
