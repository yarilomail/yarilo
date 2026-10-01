package mdbox

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// metricReadRefreshed counts reads whose file was gone, so the map was reloaded:
// how often a session meets a purge from elsewhere.
var metricReadRefreshed = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: "yarilo",
	Subsystem: "mdbox",
	Name:      "read_refreshed_after_purge_total",
	Help:      "Message reads whose storage file was gone under a cached map, so the map was reloaded and the read retried once.",
})
