package mailboxbase

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The names are the ones the reconcile has always carried. It is no longer an
// IMAP-only pass, so they no longer describe where it runs; renaming a series
// is a decision of its own and is not taken here.
var (
	MetricReconcile = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "imap_maildir_sync_total",
		Help: "Maildir reconcile decisions: scanned means cur/ and new/ were walked, skipped means the change token said nothing had changed.",
	}, []string{"result"}) // scanned | skipped

	MetricReconcileSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "imap_maildir_sync_seconds",
		Help:    "Time one maildir proactive reconcile took, from computing the change token through the index update.",
		Buckets: prometheus.ExponentialBuckets(0.0001, 4, 11), // 100us .. ~100s
	})
)
