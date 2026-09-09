package lmtp

import (
	"errors"
	"fmt"
	"testing"
)

// A refused delivery names the leg it stopped at: the sender sees 451 and
// nothing, so the leg is the only place the reason exists.
func TestTheRefusedLegIsNamed(t *testing.T) {
	addr := "10.0.0.1:10024"
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("lmtp/proxy: connect %s: %w", addr, errors.New("refused")), "connect"},
		{fmt.Errorf("lmtp/proxy: LHLO %s: %w", addr, errors.New("EOF")), "lhlo"},
		{fmt.Errorf("lmtp/proxy: MAIL FROM %s: %w", addr, errors.New("450")), "mail-from"},
		{fmt.Errorf("lmtp/proxy: RCPT TO %s: %w", addr, errors.New("550")), "rcpt-to"},
		{errors.New("something else entirely"), "other"},
		{nil, "none"},
	} {
		if got := proxyLeg(tc.err); got != tc.want {
			t.Errorf("proxyLeg(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// The line names the backend a recipient was routed to, which is the other half
// of a failure the sender cannot see.
func TestTheBackendOfARecipientIsNamed(t *testing.T) {
	p := &proxyRouter{}
	per := map[string][]string{
		"10.0.0.1:10024": {"a@x", "b@x"},
		"10.0.0.2:10024": {"c@x"},
	}
	if got := p.backendFor("c@x", per); got != "10.0.0.2:10024" {
		t.Errorf("backend %q, want 10.0.0.2:10024", got)
	}
	if got := p.backendFor("nobody@x", per); got != "" {
		t.Errorf("an unrouted recipient named backend %q", got)
	}
}
