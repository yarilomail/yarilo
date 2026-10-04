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

// metricChainReread counts records read because a store had no read before it
// in the same handle. Every other store checksums from memory (#1714).
var metricChainReread = promauto.NewCounter(prometheus.CounterOpts{
	Name: "index_cache_chain_reread_total",
	Help: "Cache record chains read again because a field was stored without a read of that message first.",
})
