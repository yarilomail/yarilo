package msgcache

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
)

// A FETCH that misses reads the message's chain once: the store that follows
// checksums from what the read already returned. Reading it again to checksum
// it cost a fifth of the backend's CPU on the stand (#1714).
func TestAMissReadsTheChainOnce(t *testing.T) {
	idx, f, m := compatFolder(t)
	hdr := craftedHeader(t)

	// A record of headers only, which is what a cache from the reference
	// carries: the envelope is built and written back on the first read.
	fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fc == nil {
		t.Fatal("cache unavailable")
	}
	fc.storeEnvelopeHeaders(m, hdr)
	fc.Close()

	m = reread(t, idx, f.ID, m.UID)
	reader := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if reader == nil {
		t.Fatal("cache unavailable")
	}
	defer reader.Close()

	before := mailindex.RecordReads()
	rereads := testutil.ToFloat64(metricChainReread)
	if _, ok := reader.EnvelopeText(m); !ok {
		t.Fatal("the envelope was not built from the cached headers")
	}
	if reads := mailindex.RecordReads() - before; reads != 1 {
		t.Errorf("one miss cost %v chain reads, want 1", reads)
	}
	if now := testutil.ToFloat64(metricChainReread); now != rereads {
		t.Errorf("the store re-read the chain: counter %v -> %v", rereads, now)
	}
}

// A store with no read before it is the one case that still reads, and it is
// counted so its share is known rather than assumed.
func TestAStoreWithoutAReadIsCounted(t *testing.T) {
	idx, f, m := compatFolder(t)
	fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fc == nil {
		t.Fatal("cache unavailable")
	}
	defer fc.Close()

	was := testutil.ToFloat64(metricChainReread)
	fc.StoreSizes(m, 100, 110) // nothing read this message first
	if now := testutil.ToFloat64(metricChainReread); now != was+1 {
		t.Errorf("re-read counter = %v, want %v", now, was+1)
	}
	// And the second store of the same message does not read again.
	fc.StoreEnvelope(m, nil)
	fc.StoreSizes(m, 100, 110)
	if now := testutil.ToFloat64(metricChainReread); now != was+1 {
		t.Errorf("a later store read again: counter %v, want %v", now, was+1)
	}
}
