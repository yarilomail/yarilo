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
	before := mailindex.RecordReads()
	rereads := testutil.ToFloat64(metricChainReread)
	if _, ok := reader.EnvelopeText(m); !ok {
		t.Fatal("the envelope was not built from the cached headers")
	}
	// The write is deferred to Close, and Close is where the cost was: the
	// count is taken after it, or the row never reaches the seam.
	reader.Close()
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

// The deferred write is where the cost was, so the two cases are asserted
// through it: a head that has not moved is written without a read, and one
// that moved under the window is read once because our view of it is stale.
func TestTheDeferredWriteReadsOnlyWhenTheHeadMoved(t *testing.T) {
	tests := []struct {
		name        string
		otherWrites bool
		wantRereads float64
	}{
		{name: "the head stands still", wantRereads: 0},
		{name: "another session appended", otherWrites: true, wantRereads: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idx, f, m := compatFolder(t)
			hdr := craftedHeader(t)

			// A record to read, so the window has a chain to carry over.
			seed := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
			if seed == nil {
				t.Fatal("cache unavailable")
			}
			seed.storeEnvelopeHeaders(m, hdr)
			seed.Close()

			m = reread(t, idx, f.ID, m.UID)
			deferred := Open(idx, f.ID, Options{User: "u", Folder: f.Name, Shared: true, DeferWrites: true})
			if deferred == nil {
				t.Fatal("cache unavailable")
			}
			if _, ok := deferred.EnvelopeText(m); !ok {
				t.Fatal("the envelope was not built from the cached headers")
			}

			if tc.otherWrites {
				other := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
				if other == nil {
					t.Fatal("cache unavailable")
				}
				other.StoreSizes(reread(t, idx, f.ID, m.UID), 100, 110)
				other.Close()
			}

			was := testutil.ToFloat64(metricChainReread)
			deferred.Close() // the write happens here
			if got := testutil.ToFloat64(metricChainReread) - was; got != tc.wantRereads {
				t.Errorf("the deferred write re-read %v chains, want %v", got, tc.wantRereads)
			}

			// Whatever the path, what it wrote must read back.
			back := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
			if back == nil {
				t.Fatal("cache unavailable")
			}
			defer back.Close()
			if _, ok := back.EnvelopeText(reread(t, idx, f.ID, m.UID)); !ok {
				t.Error("the envelope the deferred window wrote does not read back")
			}
		})
	}
}
