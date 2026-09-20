package mailboxbase

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The four causes a walk can have, named as the reference names them in
// enum maildir_scan_why (maildir-sync.c:209-216).
const (
	reasonFirstSeen  = "first-seen"
	reasonTokenMoved = "token-moved"
	reasonHotNew     = "hot-new"
	reasonOwed       = "owed"
)

// The names the reconcile has always carried. It is no longer IMAP-only, and
// renaming a series is a decision of its own, not taken here.
var (
	MetricReconcile = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "imap_maildir_sync_total",
		Help: "Maildir reconcile decisions, with what drove a walk: first-seen is a folder this process had not seen, token-moved its mtime or size changed, hot-new its arrival directory was written this second, owed the window had passed on a dirty folder. A skip carries no reason. scanned means the change token moved and cur/ and new/ were walked, scanned-untokened means there was no token to compare, skipped means the token said nothing had changed, skipped-window means it moved inside the window the mtime cannot vouch for and the folder was walked less than a window ago.",
	}, []string{"result", "reason"}) // scanned | scanned-untokened | skipped | skipped-window

	MetricReconcileSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "imap_maildir_sync_seconds",
		Help:    "Time one maildir proactive reconcile took, from computing the change token through the index update.",
		Buckets: prometheus.ExponentialBuckets(0.0001, 4, 11), // 100us .. ~100s
	})
)
