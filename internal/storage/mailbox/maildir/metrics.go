package maildir

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The driver's half of the exclusion, by the call that took it: the uidlist
// here, the journal in the index (#1840). Sites are named by role.
var metricLockAcquired = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "maildir_lock_acquired_total",
	Help: "Locks the maildir driver took on the uidlist, by the call that took it. A store, a rename and a read take none.",
}, []string{"site"})

// Held, not waited for: the cost a site imposes on the folder is the time it
// keeps the lock, which no count of acquisitions carries (#1809).
var metricLockHold = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "maildir_lock_hold_seconds",
	Help:    "Time one call held the uidlist, by the call that took it. Pairs with fileindex_lock_hold_seconds, which measures the journal.",
	Buckets: prometheus.ExponentialBuckets(0.0001, 4, 10),
}, []string{"site"})

// metricImportRowRefused counts imports skipped because the list already names
// the file under another uid: one message reported, not a batch lost (#1745).
var metricImportRowRefused = promauto.NewCounter(prometheus.CounterOpts{
	Name: "maildir_import_row_refused_total",
	Help: "Reconcile imports skipped because the list names the file under another uid. The message stays with its owner; nothing is written for the skipped one.",
})

// metricRemoveMiss counts removals that found no file even after re-reading the
// listing: the message was already gone. Zero is the expected reading (#1797).
var metricRemoveMiss = promauto.NewCounter(prometheus.CounterOpts{
	Name: "maildir_remove_miss_total",
	Help: "Removals that found no file under the name given or the name the listing now shows. Each is a message something else removed first.",
})

// metricRecordWithoutRow counts records met with no row in the list, deduped
// per record per process. Zero is the expected reading (#1745).
var metricRecordWithoutRow = promauto.NewCounter(prometheus.CounterOpts{
	Name: "maildir_record_without_row_total",
	Help: "Index records whose message the list does not name, counted once per record per process. Each is a message a client sees and cannot read.",
})

const (
	lockSiteSave           = "save"            // a delivery or APPEND
	lockSiteSweepTemps     = "sweep-temps"     // clearing what a save never published
	lockSiteWriteFlags     = "write-flags"     // one message's flags reaching its name
	lockSiteWriteFlagsBulk = "write-flags-all" // a whole STORE's worth at once
	lockSiteMove           = "move"            // between folders
	lockSiteCreate         = "create"
	lockSiteDelete         = "delete"
	lockSiteKeywords       = "keyword-letters" // allocating a keyword letter outside a write
	lockSiteReconcileMove  = "reconcile-move"  // new/ into cur/
	lockSiteReconcileApply = "reconcile-apply" // the index write a reconcile decided on
	lockSiteMigrateNames   = "migrate-names"   // the pass that puts records in the list
	lockSiteRename         = "rename"          // a folder rename, both locks
)
