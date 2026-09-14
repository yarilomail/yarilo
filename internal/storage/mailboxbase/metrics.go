package mailboxbase

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The names the reconcile has always carried. It is no longer IMAP-only, and
// renaming a series is a decision of its own, not taken here.
var (
	MetricReconcile = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "imap_maildir_sync_total",
		Help: "Maildir reconcile decisions: scanned means the change token moved and cur/ and new/ were walked, scanned-untokened means there was no token to compare, skipped means the token said nothing had changed.",
	}, []string{"result"}) // scanned | scanned-untokened | skipped

	MetricReconcileSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "imap_maildir_sync_seconds",
		Help:    "Time one maildir proactive reconcile took, from computing the change token through the index update.",
		Buckets: prometheus.ExponentialBuckets(0.0001, 4, 11), // 100us .. ~100s
	})
)
