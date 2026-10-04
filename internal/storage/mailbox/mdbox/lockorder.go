package mdbox

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// metricLockOrderRefused counts folder locks refused because this goroutine
// already holds the map. Non-zero means a caller is reintroducing #1884.
var metricLockOrderRefused = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "yarilo",
	Subsystem: "mdbox",
	Name:      "lock_order_refused_total",
	Help:      "Folder locks refused under a held map lock, by site.",
}, []string{"site"})
