package lmtp

import (
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// proxyRefused counts proxied deliveries the backend leg refused, by the step
// that refused them: a 451 reaching the sender otherwise carries no reason.
var proxyRefused = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "lmtp_proxy_delivery_refused_total",
	Help: "Proxied LMTP deliveries that failed, by the leg of the backend transaction that failed. A count under connect or lhlo means the backend was never spoken to properly — usually the wrong port for this protocol.",
}, []string{"leg"})

// proxyLeg names the step an error came from. The legs are the transaction's
// own, so the label set is closed.
func proxyLeg(err error) string {
	if err == nil {
		return "none"
	}
	msg := err.Error()
	for _, leg := range []string{"connect", "LHLO", "MAIL FROM", "RCPT TO", "DATA", "write", "close"} {
		if strings.Contains(msg, "lmtp/proxy: "+leg) {
			return strings.ToLower(strings.ReplaceAll(leg, " ", "-"))
		}
	}
	return "other"
}

// backendFor is the address a recipient was routed to, for a line that has to
// name it. Empty when the routing is no longer known.
func (p *proxyRouter) backendFor(rcpt string, perBackend map[string][]string) string {
	for addr, rcpts := range perBackend {
		for _, r := range rcpts {
			if r == rcpt {
				return addr
			}
		}
	}
	return ""
}
