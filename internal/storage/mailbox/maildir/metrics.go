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

// metricPartialEmpty counts arrivals passes that found nothing to move: one
// readdir of an empty directory, which is what they cost (#1952).
var metricPartialEmpty = promauto.NewCounter(prometheus.CounterOpts{
	Name: "maildir_partial_pass_empty_total",
	Help: "Arrivals-only passes that found nothing in new/ to move, so they read an empty directory and stopped.",
})

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

// Where a listing's cost comes from: the uid list, read whole or by its tail,
// and cur/, by whoever asked for it (#1875).
var metricUIDListRead = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "maildir_uidlist_read_total",
	Help: "Reads of a folder's uid list, by how much was parsed: whole is every row, tail is only the rows appended since the last read.",
}, []string{"mode"})

var metricDirRead = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "maildir_dir_read_total",
	Help: "Reads of a folder's cur/ off disk, by what asked for it: current-name is a record being named, scan is a walk, remove is an unlink checking what it took.",
}, []string{"reason"})

// Why a name lookup could not use the cached listing: one read per login and
// one per FETCH are otherwise the same number (#1875).
var metricListingMiss = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "maildir_listing_miss_total",
	Help: "Name lookups that could not use the cached listing, by why: no-listing is nothing cached yet, stale-mtime is a directory that changed under it.",
}, []string{"reason"})

// What a name that moved on cost: one re-sync and one second attempt, at the
// consumer that found it out (#1987).
var metricListingRetry = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "maildir_listing_retry_total",
	Help: "Re-syncs of a folder's listing after a name taken from it had moved on, by where it was found out: path is a name lookup, rename is a flag write.",
}, []string{"at"})

// Every stat a name lookup makes to check a cache it already has: the
// reference checks once per sync, not once per message (#1875).
var metricCacheStat = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "maildir_cache_stat_total",
	Help: "Stats made to decide whether a cached listing or uid list is still current, by what was stated.",
}, []string{"what"})

// Who closes the window a walk earned: a window shut by our own write is one
// the next lookup pays a stat for, and nobody asked it to (#1875).
var metricWindowClosed = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "maildir_window_closed_total",
	Help: "Times a folder's checked window was closed, by what closed it.",
}, []string{"by"})

// Whether the stamp in the index opened the window: a partial pass never walks
// cur/, so without it every lookup states the uid list (#1875).
var (
	metricStampHit = promauto.NewCounter(prometheus.CounterOpts{
		Name: "maildir_uidlist_stamp_hit_total",
		Help: "Folder opens where the uid list matched the stamp the index was built from, so the window opened without a walk.",
	})
	metricStampMiss = promauto.NewCounter(prometheus.CounterOpts{
		Name: "maildir_uidlist_stamp_miss_total",
		Help: "Folder opens where the uid list had moved on from the stamp, or there was none.",
	})
)
