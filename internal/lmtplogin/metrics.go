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
