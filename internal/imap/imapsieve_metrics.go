package imap

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	dto "github.com/prometheus/client_model/go"
)

// metricImapSieveLookupErrors counts events that ran without imapsieve because
// the annotation dict failed: invisible in the log alone, on a dashboard here.
var metricImapSieveLookupErrors = promauto.NewCounter(prometheus.CounterOpts{
	Name: "yarilo_imapsieve_lookup_errors_total",
	Help: "Stored-message events that skipped imapsieve because the annotation lookup failed.",
})

// ImapSieveLookupErrors reports the counter, for tests: a metric nobody can read
// back is a metric nobody can assert.
func ImapSieveLookupErrors() float64 {
	m := &dto.Metric{}
	if err := metricImapSieveLookupErrors.Write(m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}
