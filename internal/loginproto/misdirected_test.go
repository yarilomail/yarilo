package loginproto

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	masterclient "github.com/yarilomail/yarilo/pkg/authclient"
)

// counterFor reads one reason off the refusal counter.
func counterFor(t *testing.T, reason string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "loginproto_preamble_rejected_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "reason" && l.GetValue() == reason {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// A peer speaking the bare protocol to this port is named by its own reason:
// every session it opens is refused, and the sender sees only a failure code.
func TestRefusalReasonsAreToldApart(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"bare protocol", ErrNotYarilo, "no-preamble"},
		{"auth down", masterclient.ErrUnavailable, "auth-unavailable"},
		{"read timed out", &net.OpError{Op: "read", Err: timeoutErr{}}, "timeout"},
		{"anything else", errors.New("short write"), "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := refusalReason(tc.err); got != tc.want {
				t.Errorf("reason %q, want %q", got, tc.want)
			}
		})
	}
}

// The refusal is counted where it happens, so an operator reading a bare 451
// has a number even when the line is debug.
func TestAMisdirectedConnectionIsCounted(t *testing.T) {
	l := &PreambleListener{}
	before := counterFor(t, "no-preamble")
	l.noteHandshakeFailure(nopConn{}, ErrNotYarilo)
	if got := counterFor(t, "no-preamble") - before; got != 1 {
		t.Errorf("the refusal was counted %v times, want 1", got)
	}
}

// nopConn is an address and nothing else: the refusal path only asks for one.
type nopConn struct{ net.Conn }

func (nopConn) RemoteAddr() net.Addr { return &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 4242} }

// timeoutErr is what a deadline gives: a net.Error that says it timed out.
type timeoutErr struct{}

func (timeoutErr) Error() string { return "i/o timeout" }
func (timeoutErr) Timeout() bool { return true }

// A wrapped cause is still told apart: the reason comes from the error's type,
// not from words in its text.
func TestAWrappedCauseKeepsItsReason(t *testing.T) {
	wrapped := fmt.Errorf("preamble: %w", ErrNotYarilo)
	if got := refusalReason(wrapped); got != "no-preamble" {
		t.Errorf("reason %q for a wrapped cause, want no-preamble", got)
	}
	dressed := fmt.Errorf("read tcp 10.0.0.1: %w", &net.OpError{Op: "read", Err: timeoutErr{}})
	if got := refusalReason(dressed); got != "timeout" {
		t.Errorf("reason %q for a wrapped timeout, want timeout", got)
	}
	// A message that merely says the words is not a reason.
	if got := refusalReason(errors.New("the handshake timeout was mentioned")); got != "other" {
		t.Errorf("reason %q for a text-only match, want other", got)
	}
}
