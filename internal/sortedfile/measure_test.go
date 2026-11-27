package sortedfile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// threadRecords is a synthetic account of n messages as the thread store
// holds it: M (Message-ID to thread), G (GUID to thread), T (thread and GUID),
// and S (subject key to thread) for one message in three.
func threadRecords(n int, from int) map[uint32][]KV {
	hexOf := func(s string) []byte {
		sum := sha256.Sum256([]byte(s))
		return []byte(hex.EncodeToString(sum[:16]))
	}
	out := map[uint32][]KV{}
	for i := from; i < from+n; i++ {
		guid := hexOf("g" + strconv.Itoa(i))
		thread := hexOf("t" + strconv.Itoa(i/3))
		msgid := []byte(fmt.Sprintf("<%d.%s@mail.example-sender-domain.org>", i, guid[:16]))
		out[1] = append(out[1], KV{msgid, thread})
		out[2] = append(out[2], KV{guid, thread})
		out[3] = append(out[3], KV{append(append([]byte(nil), thread...), guid...), nil})
		if i%3 == 0 {
			out[4] = append(out[4], KV{[]byte(fmt.Sprintf("subject of conversation number %d", i/3)), thread})
		}
	}
	for id := range out {
		out[id] = SortKVs(out[id])
	}
	return out
}

var sections = []uint32{1, 2, 3, 4}

func writeAll(t *testing.T, path string, base *Reader, recs map[uint32][]KV, gen uint64) {
	t.Helper()
	w, err := Create(path, gen, 0, sections, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range sections {
		if err := MergeSection(w, base, id, recs[id]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
}

// The numbers THREADS-STORE.md §4 asks for: size, time and peak memory of
// one rewrite of a synthetic base of 100k and 1M messages with a tail of 1000
// deliveries. YARILO_MEASURE_THREADS=1 runs it; the gate does not.
func TestMeasureABaseRewrite(t *testing.T) {
	if os.Getenv("YARILO_MEASURE_THREADS") == "" {
		t.Skip("YARILO_MEASURE_THREADS=1 measures")
	}
	for _, n := range []int{100000, 1000000} {
		dir := t.TempDir()
		path := filepath.Join(dir, "threads.base")
		writeAll(t, path, nil, threadRecords(n, 0), 1)
		st, _ := os.Stat(path)
		base, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		tail := threadRecords(1000, n)
		runtime.GC()
		var peak atomic.Uint64
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			var m runtime.MemStats
			for {
				runtime.ReadMemStats(&m)
				if m.HeapInuse > peak.Load() {
					peak.Store(m.HeapInuse)
				}
				select {
				case <-stop:
					return
				case <-time.After(5 * time.Millisecond):
				}
			}
		}()
		var m0 runtime.MemStats
		runtime.ReadMemStats(&m0)
		start := time.Now()
		writeAll(t, filepath.Join(dir, "next"), base, tail, 2)
		took := time.Since(start)
		close(stop)
		<-done
		base.Close() //nolint:errcheck
		nst, _ := os.Stat(filepath.Join(dir, "next"))
		t.Logf("%d messages: base %.1f MB (%.0f B a message), rewrite %v, written %.1f MB, heap in use before %.1f MB, peak %.1f MB",
			n, float64(st.Size())/1e6, float64(st.Size())/float64(n), took.Round(time.Millisecond),
			float64(nst.Size())/1e6, float64(m0.HeapInuse)/1e6, float64(peak.Load())/1e6)
	}
}
