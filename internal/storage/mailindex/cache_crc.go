package mailindex

import (
	"encoding/binary"
	"hash/crc32"
	"sort"
)

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
