package ftsproto

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Retractions refused because the client speaks protocol 1, whose EXPUNGE
// carries no message GUID. Non-zero while a rollout has both versions (#1986).
var metricExpungeRefused = promauto.NewCounter(prometheus.CounterOpts{
	Name: "fts_expunge_old_protocol_total",
	Help: "EXPUNGE commands refused for arriving in the form without a message guid.",
})
