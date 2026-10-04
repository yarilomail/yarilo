package dictsrv

import "github.com/prometheus/client_golang/prometheus"

// Metrics counts dict work by name and driver: an operator asking "which dict
// is slow" needs the name, and "which engine" needs the driver (#1733).
type Metrics struct {
	ops     *prometheus.CounterVec
	seconds *prometheus.HistogramVec
}

// NewMetrics registers the dict service's metrics.
func NewMetrics(r prometheus.Registerer) *Metrics {
	m := &Metrics{
		ops: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "yarilo_dict_operations_total",
			Help: "Dict operations served, by dict name, driver, operation and result.",
		}, []string{"dict", "driver", "op", "result"}),
		seconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "yarilo_dict_operation_seconds",
			Help:    "Latency of a dict operation inside the service, by dict name and operation.",
			Buckets: prometheus.ExponentialBuckets(0.0001, 3, 10), // 100µs … ~2s
		}, []string{"dict", "op"}),
	}
	if r != nil {
		r.MustRegister(m.ops, m.seconds)
	}
	return m
}
