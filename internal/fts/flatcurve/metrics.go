//go:build flatcurve

package flatcurve

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Optimize metrics, registered on the default registry.
var (
	metricOptimizeRuns = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fts_optimize_runs_total",
		Help: "Completed flatcurve shard-optimize runs (manual or automatic).",
	})
	metricOptimizeShardsMerged = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fts_optimize_shards_merged_total",
		Help: "Shards merged across all flatcurve optimize runs.",
	})
)

// Documents of one message folded together by a compaction: a copy indexed in
// another shard has a document of its own until then (#1986).
var metricDedupMerged = promauto.NewCounter(prometheus.CounterOpts{
	Name: "fts_flatcurve_dedup_merged_total",
	Help: "Documents merged into another document of the same message during compaction.",
})
