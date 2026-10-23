package counters

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// metricBumps counts the counter's raises in this process: per write command it
// says how often a write path moves the counter.
var metricBumps = promauto.NewCounter(prometheus.CounterOpts{
	Name: "counters_bump_total",
	Help: "Raises of an account's change counter in this process.",
})
