package msgcache

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Zero is the expected reading: a mismatch is a record read for the wrong
// message, which no other signal makes visible (#1714).
var metricCRCMismatch = promauto.NewCounter(prometheus.CounterOpts{
	Name: "index_cache_record_crc_mismatch_total",
	Help: "Cache records whose crc32 did not match the index's. Each one was recomputed from the message and rewritten.",
})
