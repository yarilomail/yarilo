package lmtplogin

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Phase labels for rcptPhaseSeconds, in the order one RCPT walks them (#2149).
const (
	phaseUserdb         = "userdb"
	phaseDirectorDial   = "director_dial"
	phaseDirectorLookup = "director_lookup"
	phaseWardenDial     = "warden_dial"
	phaseWardenLookup   = "warden_lookup"
	phaseWardenConnect  = "warden_connect"
	phaseToken          = "token"
)

// rcptPhaseSeconds starts at 0.1ms: most phases are sub-millisecond when healthy.
var rcptPhaseSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "yarilo",
	Subsystem: "lmtp_login",
	Name:      "rcpt_phase_seconds",
	Help:      "Latency of one RCPT phase (userdb, director_dial, director_lookup, warden_dial, warden_lookup, warden_connect, token).",
	Buckets:   prometheus.ExponentialBuckets(0.0001, 2, 20), // 0.1ms … ~52s
}, []string{"phase"})

func observeRcptPhase(phase string, start time.Time) {
	rcptPhaseSeconds.WithLabelValues(phase).Observe(time.Since(start).Seconds())
}

// Phase labels for backendPhaseSeconds: the leg to the backend that runs on
// DATA, one connection per recipient of each message (docs-internal#318).
const (
	backendPhaseDial  = "dial"  // TCP and, with internal TLS, the whole handshake
	backendPhaseSetup = "setup" // preamble, LHLO, MAIL and RCPT, to the accepted RCPT
	backendPhaseData  = "data"  // DATA to the backend's reply for the recipient
)

var backendPhaseSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "yarilo",
	Subsystem: "lmtp_login",
	Name:      "backend_phase_seconds",
	Help:      "Latency of one phase of the delivery to the backend on DATA (dial, setup, data), one connection per recipient.",
	Buckets:   prometheus.ExponentialBuckets(0.0001, 2, 20), // 0.1ms … ~52s
}, []string{"phase"})

// backendTLSHandshakes tells a full handshake from a resumed one: without it
// the dial phase cannot say whether each message pays the full price.
var backendTLSHandshakes = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "yarilo",
	Subsystem: "lmtp_login",
	Name:      "backend_tls_handshakes_total",
	Help:      "TLS handshakes with the backend, by whether the session was resumed.",
}, []string{"resumed"})

func observeBackendPhase(phase string, start time.Time) {
	backendPhaseSeconds.WithLabelValues(phase).Observe(time.Since(start).Seconds())
}
