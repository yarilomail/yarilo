package loginproto

import (
	"errors"
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
		{"read timed out", errors.New("read tcp: i/o timeout"), "timeout"},
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
