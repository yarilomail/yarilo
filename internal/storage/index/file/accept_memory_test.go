package file

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// INDEX-MEMORY.md §7, as the slope between two folders. A marked row that
// starts passing fails, so the step that fixes it drops its mark.
var acceptExpectedFail = map[int]bool{2: true, 3: true, 6: true, 7: true}

const (
	acceptSmall = 2000
	acceptLarge = 10000
	// acceptRecord is a record's size in the folder acceptFolder writes.
	acceptRecord = 56
	// acceptNoise is what "no growth" allows a message, 256 KiB over the
	// 8000 messages between the folders: the heap's noise between two GCs.
	acceptNoise = 32
)

func acceptance(t *testing.T, row int, ok bool, measured string) {
	t.Helper()
	switch {
	case acceptExpectedFail[row] && ok:
		t.Errorf("acceptance %d now passes (%s): drop its expected-fail mark", row, measured)
	case acceptExpectedFail[row]:
		t.Logf("acceptance %d, expected to fail until its step lands: %s", row, measured)
	case !ok:
		t.Errorf("acceptance %d: %s", row, measured)
	}
}

func heapInUse() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapInuse
}

// acceptFolder writes a home whose INBOX base holds n records as a delivery
// writes them, straight through mailindex: filling it message by message
// would cost the gate seconds a row.
func acceptFolder(t *testing.T, n int) string {
	t.Helper()
	home := t.TempDir()
	h := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home})
	f, err := h.OpenFolder("INBOX", 7)
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := h.AllocateUID(f.ID)
	if err := h.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, GUID: [16]byte{1}, Size: 1200, VSize: 1230, InternalDate: time.Unix(1700000000, 0), Flags: []string{`\Seen`}}); err != nil {
		t.Fatal(err)
	}
	fs, err := h.(*userHandle).ui.state(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	if err := fs.flush(); err != nil {
		t.Fatal(err)
	}
	path := fs.indexPath
	fs.mu.Unlock()
	h.Close() //nolint:errcheck
	base, err := mailindex.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := base.Records[0]
	recs := make([]*mailindex.Record, n)
	for i := range recs {
		r := &mailindex.Record{UID: uint32(i + 1), Flags: tmpl.Flags, Ext: map[string][]byte{}}
		for k, v := range tmpl.Ext {
			r.Ext[k] = append([]byte(nil), v...)
		}
		if g, ok := r.Ext[extNameGUID]; ok {
			binary.LittleEndian.PutUint32(g, uint32(i+1))
		}
		recs[i] = r
	}
	hdr := base.Header
	hdr.NextUID = uint32(n + 1)
	hdr.MessagesCount = uint32(n)
	hdr.SeenMessagesCount = uint32(n)
	if _, err := mailindex.Recreate(mailindex.RecreateInput{Path: path, Header: hdr, Extensions: base.Extensions, Records: recs}); err != nil {
		t.Fatal(err)
	}
	// The log describes the one-message base; this one is the whole folder.
	if err := os.Remove(path + ".log"); err != nil {
		t.Fatal(err)
	}
	return home
}

func acceptOpen(t *testing.T, home string) (mailbox.UserIndex, *mailbox.Folder) {
	t.Helper()
	h := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home})
	f, err := h.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	return h, f
}

func slope(small, large uint64) float64 {
	return (float64(large) - float64(small)) / (acceptLarge - acceptSmall)
}

// sessionHeld is the heap a session holds with every record of a folder of n
// read, and what is left of it after close.
func sessionHeld(t *testing.T, n int) (held, left uint64) {
	t.Helper()
	home := acceptFolder(t, n)
	before := heapInUse()
	h, f := acceptOpen(t, home)
	msgs, err := h.GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil || len(msgs) != n {
		t.Fatalf("read %d records of %d: %v", len(msgs), n, err)
	}
	held = heapInUse() - before
	h.Close() //nolint:errcheck
	return held, max(heapInUse(), before) - before
}

// 1: a session holds at most 1.5 records a message. 3: closing it gives that
// back.
func TestAcceptASessionHoldsItsFolderAsBytes(t *testing.T) {
	heldS, leftS := sessionHeld(t, acceptSmall)
	heldL, leftL := sessionHeld(t, acceptLarge)
	held := slope(heldS, heldL)
	acceptance(t, 1, held <= 1.5*acceptRecord, fmt.Sprintf("%.0f B a message held, bound %.0f", held, 1.5*acceptRecord))
	left := slope(leftS, leftL)
	acceptance(t, 3, left <= acceptNoise, fmt.Sprintf("%.0f B a message left after close, bound %d", left, acceptNoise))
}

// deliveryAlloc is what the first delivery of a connection into a folder of
// n allocates: a bound on its peak that needs no sampler stopping the world.
func deliveryAlloc(t *testing.T, n int) (uint64, time.Duration) {
	t.Helper()
	home := acceptFolder(t, n)
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	start := time.Now()
	h, f := acceptOpen(t, home)
	uid, _ := h.AllocateUID(f.ID)
	err := h.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, GUID: [16]byte{2}, Size: 1200, VSize: 1230, InternalDate: time.Unix(1700000000, 0)})
	h.Close() //nolint:errcheck
	took := time.Since(start)
	runtime.ReadMemStats(&m1)
	if err != nil {
		t.Fatal(err)
	}
	return m1.TotalAlloc - m0.TotalAlloc, took
}

// 2: a delivery reads no record: what it allocates does not grow with the
// folder. Its time is logged, not bounded; the stand measures it in step 5.
func TestAcceptADeliveryReadsNoRecord(t *testing.T) {
	allocS, _ := deliveryAlloc(t, acceptSmall)
	allocL, took := deliveryAlloc(t, acceptLarge)
	grow := slope(allocS, allocL)
	acceptance(t, 2, grow <= acceptNoise,
		fmt.Sprintf("allocation grows %.0f B a message, bound %d; %s into %d", grow, acceptNoise, took.Round(time.Millisecond), acceptLarge))
}

// 5: in one session the 1000th flag change costs within 10 % of the first, in
// bytes allocated; its time is logged, not bounded. develop: 5.5 KB a change.
func TestAcceptAChangeCostsTheSameLate(t *testing.T) {
	home := acceptFolder(t, acceptLarge)
	h, f := acceptOpen(t, home)
	defer h.Close() //nolint:errcheck
	change := func(uid uint32) (time.Duration, uint64) {
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		start := time.Now()
		tx, err := h.Begin(f.ID)
		if err != nil {
			t.Fatal(err)
		}
		tx.UpdateFlags(uid, mailbox.FlagsUpdate{Flags: []string{`\Flagged`}})
		if _, err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		tx.Rollback()
		took := time.Since(start)
		runtime.ReadMemStats(&m1)
		return took, m1.TotalAlloc - m0.TotalAlloc
	}
	avg := func(from uint32) (time.Duration, uint64) {
		var tsum time.Duration
		var bsum uint64
		for i := uint32(0); i < 10; i++ {
			d, b := change(from + i*97)
			tsum += d
			bsum += b
		}
		return tsum / 10, bsum / 10
	}
	firstT, firstB := avg(1)
	for i := uint32(0); i < 980; i++ {
		change(2000 + i*7)
	}
	lateT, lateB := avg(9000)
	ok := float64(lateB) <= 1.1*float64(firstB)
	acceptance(t, 5, ok, fmt.Sprintf("first ten %s and %d B each, after 990 more %s and %d B each", firstT, firstB, lateT, lateB))
}
