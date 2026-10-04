package mailindex

import (
	"encoding/binary"
	"hash/crc32"
	"sort"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	dto "github.com/prometheus/client_model/go"
)

// metricRecordReads counts chains read off the cache file. One per message per
// FETCH is the shape a listing should have (#1714).
var metricRecordReads = promauto.NewCounter(prometheus.CounterOpts{
	Name: "index_cache_record_read_total",
	Help: "Cache record chains read from the file, one per walk of a message's chain.",
})

// RecordReads is what the counter holds, for a row that asserts how many reads
// one FETCH costs.
func RecordReads() float64 {
	m := &dto.Metric{}
	if err := metricRecordReads.Write(m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// RecordCRC hashes a record's merged field values -- ids sorted, each with its
// length -- and is what the index stores beside the record's offset (#1714).
func RecordCRC(vals map[uint32][]byte) uint32 {
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
