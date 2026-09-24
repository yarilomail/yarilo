package file

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// What a read command costs inside the index, as a whole and in named parts.
//
// The folder index is the one layer maildir and sdbox share, which is where the
// 24-32% gap to the reference has to live: the two drivers have nothing else in
// common below the session (#1229). None of it was measured — the lock path
// logged at debug level and nothing else was timed at all — so a per-command
// number could say a command costs 25ms without saying whether that is the
// round trip to the lock service, the freshness check, or rebuilding the
// folder's message list for the tenth time in a row.
//
// The parts are observed only inside the whole, and what the whole holds beyond
// them is the finding. It cannot be negative, and a test asserts it.
var (
	// A read that fails before it builds its answer reports the parts it
	// reached and not the ones it did not, so the counts of the parts are not
	// equal to each other or to the count of the whole. The sums still
	// reconcile, which is what the split is for.
	metricReadSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "fileindex_read_seconds",
		Help:    "Time one read of a folder's state took, whole: lock, freshness check and building the answer.",
		Buckets: prometheus.ExponentialBuckets(0.00001, 4, 11), // 10us .. ~10s
	})
	metricReadPart = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fileindex_read_part_seconds",
		Help:    "Time in one part of a folder read: lock, reload or build.",
		Buckets: prometheus.ExponentialBuckets(0.00001, 4, 11),
	}, []string{"part"})

	// Held, not waited for: without it the wait says who queued and nothing
	// says who made them queue (#1809).
	metricLockHold = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fileindex_lock_hold_seconds",
		Help:    "Time one write cycle held the folder journal on the volume, by mode and site. Pairs with maildir_lock_hold_seconds, which measures the uidlist.",
		Buckets: prometheus.ExponentialBuckets(0.0001, 4, 10),
	}, []string{"mode", "site"})
	metricLockAcquired = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fileindex_lock_acquired_total",
		Help: "Journal locks taken on the volume, by mode and by the call that took it. A cycle takes one whatever it appends; a read takes none.",
	}, []string{"mode", "site"}) // shared | exclusive × read | open-probe | reload-fallback | transaction | expunge | append | write-flags | ...
	metricReload = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fileindex_reload_total",
		Help: "Folder freshness checks by outcome: adopt means a rewritten base was proven to hold what memory already held and its records were not read.",
	}, []string{"result"}) // adopt
	metricLineageStamped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fileindex_lineage_stamped_total",
		Help: "Folder indexes given a lineage on first open because they were written before the extension. Expected to rise once per folder after an upgrade and stay flat afterwards; two pods racing the same first open can each stamp it, so a folder may count twice.",
	})
)

// The sites a folder lock can be taken from. Named rather than free-form so
// "where do the remaining acquisitions come from" has a finite answer, and so a
// new call site has to choose one deliberately.
//
// The distinction that matters most is open-probe against reload-fallback:
// the first is a folder being opened, which is legitimate and bounded by
// sessions; the second is a read that wanted the lock-free path and could not
// prove freshness, which means the lineage never arrived and the migration did
// not do its job.
const (
	lockSiteOpenProbe = "open-probe"      // opening or repairing a folder
	lockSiteFallback  = "reload-fallback" // an unlocked read with nothing to prove freshness with

	// The callers of that mutation, so a total can be attributed (#1827).
	lockSiteTransaction  = "transaction"
	lockSiteFlagsDirty   = "flags-dirty"
	lockSiteRefresh      = "refresh"
	lockSitePop3Uidl     = "pop3-uidl"
	lockSiteRepairTails  = "repair-tails"
	lockSiteStampSizes   = "stamp-sizes"
	lockSiteRename       = "rename"
	lockSiteStampLineage = "stamp-lineage"
	lockSiteResetLog     = "reset-log"
	// Only a test reaches the index without a caller of its own.
	lockSiteTestWrite       = "test-write"
	lockSiteAdoptUidSpace   = "adopt-uid-space"
	lockSiteMaildirStamp    = "maildir-stamp"
	lockSiteGUIDAppend      = "guid-append"
	lockSiteAllocateUid     = "allocate-uid"
	lockSiteAppend          = "append"
	lockSiteCacheExtension  = "cache-extension"
	lockSiteCacheGeneration = "cache-generation"
	lockSiteCacheOffsets    = "cache-offsets"
	lockSiteCachePurge      = "cache-purge"
	lockSiteExpunge         = "expunge"
	lockSiteExpungeFloor    = "expunge-floor"
	lockSiteMarkCorrupt     = "mark-corrupt"
	lockSiteNextModseq      = "next-modseq"
	lockSiteOptimize        = "optimize"
	lockSiteRecomputeVsize  = "recompute-vsize"
	lockSiteResetFolder     = "reset-folder"
	lockSiteSaveFolder      = "save-folder"
	lockSiteSetAltTier      = "set-alt-tier"
	lockSiteSetGuids        = "set-guids"
	lockSiteVanishedGuids   = "vanished-guids"
	lockSiteWriteFlags      = "write-flags"
)

// observeReadPart records one named part of a read. Guarded by the caller
// knowing it is inside a read: the same functions are reached from write paths,
// where there is no whole for a part to sit inside, and a part counted outside
// its whole makes the remainder go negative and say nothing.
func observeReadPart(part string, d time.Duration) {
	metricReadPart.WithLabelValues(part).Observe(d.Seconds())
}

// metricJournalWriteFailed counts the appends the journal refused, by why: a
// volume filling up is otherwise visible only as commands failing (#1831).
var metricJournalWriteFailed = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "fileindex_journal_write_failed_total",
	Help: "Journal appends that failed, by reason: no-space is a full volume or an exhausted disk quota, other is anything else.",
}, []string{"reason"}) // no-space | other

// metricCompactionRefused counts log compactions that could not write the base.
// Rotation stopping is invisible from the outside — the folder keeps serving
// mail while its log grows and every open replays more of it — so the count is
// what says it is happening at all (#1285).
var metricCompactionRefused = promauto.NewCounter(prometheus.CounterOpts{
	Name: "fileindex_log_compaction_refused_total",
	Help: "Log compactions that failed to rewrite the base index; rotation is not happening for those folders.",
})

// A pass over a list that has not moved: the stamp is the same, so the index
// is not written (maildir-sync-index.c:245-262).
var metricStampUnchanged = promauto.NewCounter(prometheus.CounterOpts{
	Name: "fileindex_maildir_stamp_unchanged_total",
	Help: "Maildir stamp writes that found the same stamp already recorded and left the index alone.",
})

// A copy the guid store did not take: the folder is written and the store is
// behind, which a rebuild fixes (#1711).
var metricGUIDTrackFailed = promauto.NewCounter(prometheus.CounterOpts{
	Name: "fileindex_guid_track_failed_total",
	Help: "Copies the per-user GUID store did not record after the folder write succeeded.",
})
