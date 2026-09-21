package msgcache

import (
	"encoding/binary"
	"hash/crc32"
	"sort"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Zero is the expected reading: a mismatch is a record read for the wrong
// message, which no other signal makes visible (#1714).
var metricCRCMismatch = promauto.NewCounter(prometheus.CounterOpts{
	Name: "index_cache_record_crc_mismatch_total",
	Help: "Cache records whose crc32 did not match the index's. Each one was recomputed from the message and rewritten.",
})

// recordCRC hashes a message's merged field values: ids sorted, each hashed
// with its length, so two fields swapping content do not hash the same.
func recordCRC(vals map[uint32][]byte) uint32 {
	if len(vals) == 0 {
		return 0
	}
	ids := make([]uint32, 0, len(vals))
	for id := range vals {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	h := crc32.NewIEEE()
	var head [8]byte
	for _, id := range ids {
		binary.LittleEndian.PutUint32(head[:4], id)
		binary.LittleEndian.PutUint32(head[4:], uint32(len(vals[id])))
		_, _ = h.Write(head[:])
		_, _ = h.Write(vals[id])
	}
	return h.Sum32()
}
