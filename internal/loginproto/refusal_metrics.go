package loginproto

import (
	"errors"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	masterclient "github.com/yarilomail/yarilo/pkg/authclient"
)

// preambleRejected counts connections refused before a session began, by
// reason: at debug the line is invisible, the number is not.
var preambleRejected = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "loginproto_preamble_rejected_total",
	Help: "Connections refused before a session started, by reason. no-preamble means a peer spoke the bare protocol to a port that expects the internal preamble — an MTA or proxy pointed at the wrong port refuses every delivery this way.",
}, []string{"reason"})

// refusalReason names why a handshake ended, from the closed set the label may
// carry.
func refusalReason(err error) string {
	switch {
	case errors.Is(err, ErrNotYarilo):
		return "no-preamble"
	case errors.Is(err, masterclient.ErrUnavailable):
		return "auth-unavailable"
	case err == nil:
		return "none"
	case strings.Contains(err.Error(), "handshake"):
		return "tls"
	case strings.Contains(err.Error(), "timeout"):
		return "timeout"
	}
	return "other"
}
