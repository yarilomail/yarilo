package master

import "github.com/prometheus/client_golang/prometheus"

type metrics struct {
	restarts     *prometheus.CounterVec
	up           *prometheus.GaugeVec
	started      *prometheus.GaugeVec
	logDropped   *prometheus.CounterVec
	logTruncated *prometheus.CounterVec
}

func newMetrics(r prometheus.Registerer) *metrics {
	m := &metrics{
		restarts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "yarilo_master_restarts_total",
			Help: "Components started again after they exited, by component and why: exit (a status), signal, or requested.",
		}, []string{"component", "reason"}),
		up: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "yarilo_master_component_up",
			Help: "1 while the component's process runs.",
		}, []string{"component"}),
		started: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "yarilo_master_component_started_timestamp_seconds",
			Help: "When the component's current process started, in Unix seconds.",
		}, []string{"component"}),
		logDropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "yarilo_master_log_dropped_total",
			Help: "Lines of a component's output dropped because its log could not take them.",
		}, []string{"component"}),
		logTruncated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "yarilo_master_log_truncated_total",
			Help: "Lines of a component's output cut to fit one syslog message.",
		}, []string{"component"}),
	}
	if r != nil {
		r.MustRegister(m.restarts, m.up, m.started, m.logDropped, m.logTruncated)
	}
	return m
}
