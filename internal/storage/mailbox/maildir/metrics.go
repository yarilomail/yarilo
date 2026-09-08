package maildir

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The driver's half of the folder lock, by the call that took it. The index
// publishes the other half on the same resource, so either alone reads as the
// whole (#1630). Sites are named by role, so moving a call keeps the number.
var metricLockAcquired = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "maildir_lock_acquired_total",
	Help: "Cross-process folder locks the maildir driver acquired, by the call that took it. Shares the resource with fileindex_lock_acquired_total; the sum of the two is what contends on one folder.",
}, []string{"site"})

// metricImportRowRefused counts imports skipped because the list already names
// the file under another uid: one message reported, not a batch lost (#1745).
var metricImportRowRefused = promauto.NewCounter(prometheus.CounterOpts{
	Name: "maildir_import_row_refused_total",
	Help: "Reconcile imports skipped because the list names the file under another uid. The message stays with its owner; nothing is written for the skipped one.",
})

// metricRecordWithoutRow counts records met with no row in the list, deduped
// per record per process. Zero is the expected reading (#1745).
var metricRecordWithoutRow = promauto.NewCounter(prometheus.CounterOpts{
	Name: "maildir_record_without_row_total",
	Help: "Index records whose message the list does not name, counted once per record per process. Each is a message a client sees and cannot read.",
})

const (
	lockSiteSave           = "save"            // a delivery or APPEND
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
