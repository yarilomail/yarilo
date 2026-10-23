package counters

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// metricBumps counts the counter's raises in this process by write path, so a
// window can say which path raised it per command.
var metricBumps = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "counters_bump_total",
	Help: "Raises of an account's change counter in this process, by the write path that raised it.",
}, []string{"site"})
