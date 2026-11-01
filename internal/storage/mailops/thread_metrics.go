package mailops

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// A message a save could not thread, and its recording on a later read (#2420).
var (
	metricThreadRecordFailed = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "yarilo", Subsystem: "threads", Name: "save_not_recorded_total",
		Help: "Saved messages left out of their conversation, mostly a thread lock that timed out.",
	})
	metricThreadLate = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "yarilo", Subsystem: "threads", Name: "recorded_on_read_total",
		Help: "Messages a read found missing from the sidecar and recorded into their conversation.",
	})
)
