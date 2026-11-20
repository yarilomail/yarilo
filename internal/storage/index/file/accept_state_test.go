package file

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// changeAndRead is what twenty flag changes, each followed by a read, allocate
// a change on a folder of n with a keyword defined, and how many whole-folder
// copies they made; with hold a reader keeps a view open the whole time.
func changeAndRead(t *testing.T, n int, hold bool) (alloc uint64, folderCopies int) {
	t.Helper()
	home := movedFolder(t, n)
	h, f := acceptOpen(t, home)
	defer h.Close() //nolint:errcheck
	if err := h.UpdateFlags(f.ID, 1, nil, []string{"$Work"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.GetMessages(f.ID, mailbox.SeqSet{}); err != nil {
		t.Fatal(err)
	}
	if hold {
		fs, err := h.(*userHandle).ui.state(f.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, release, err := fs.openView()
		if err != nil {
			t.Fatal(err)
		}
		defer release()
	}
	const changes = 20
	var m0, m1 runtime.MemStats
	copies := MapCopies()
	runtime.ReadMemStats(&m0)
	for i := uint32(0); i < changes; i++ {
		uid := 2 + i*97%uint32(n-2)
		if err := h.UpdateFlags(f.ID, uid, []string{`\Flagged`}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := h.GetMessages(f.ID, mailbox.SeqSet{{From: uid, To: uid}}); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&m1)
	return (m1.TotalAlloc - m0.TotalAlloc) / changes, MapCopies() - copies
}

// changeGrowth is the bytes-allocated bound, a message: wide enough for what
// other goroutines allocate meanwhile (8 B over 8000 messages is 64 KiB), and
// under a copy of the folder's records (24 B measured on develop). The
// counter is the check; this catches a copy the counter does not see.
const changeGrowth = 8

// 6 (row 5's second bound, step 3b): a change followed by a read under a
// held view makes no copy of the folder (the counter), and what it allocates
// does not grow with the folder. With no reader it already makes none.
func TestAcceptAChangeCopiesNoFolder(t *testing.T) {
	for _, hold := range []bool{false, true} {
		small, _ := changeAndRead(t, acceptSmall, hold)
		large, copies := changeAndRead(t, acceptLarge, hold)
		grow := slope(small, large)
		measured := fmt.Sprintf("held view %v: 20 changes and reads made %d whole-folder copies; a change and a read allocate %d B at %d, %d B at %d, growing %.1f B a message, bound %d",
			hold, copies, small, acceptSmall, large, acceptLarge, grow, changeGrowth)
		ok := copies == 0 && grow <= changeGrowth
		if !hold {
			if !ok {
				t.Errorf("%s", measured)
			}
			t.Log(measured)
			continue
		}
		acceptance(t, 6, ok, measured)
	}
}

// guidStoreHeld is the heap the GUID store of a mailbox of n messages holds
// once read.
func guidStoreHeld(t *testing.T, n int) uint64 {
	t.Helper()
	home := acceptFolder(t, n)
	h, f := acceptOpen(t, home)
	if _, err := h.(*userHandle).ui.RebuildGUIDStore([]uint64{f.ID}); err != nil {
		t.Fatal(err)
	}
	h.Close() //nolint:errcheck
	before := heapInUse()
	r, _ := acceptOpen(t, home)
	defer r.Close() //nolint:errcheck
	var g [16]byte
	binary.LittleEndian.PutUint32(g[:], uint32(n/2))
	if _, err := r.(*userHandle).GUIDCopies([][16]byte{g}); err != nil {
		t.Fatal(err)
	}
	return max(heapInUse(), before) - before
}

// 7: the GUID store grows with the mailbox by at most 1.5 of its records a
// message (#2625 review).
func TestAcceptTheGUIDStoreHoldsItsRecordsAsBytes(t *testing.T) {
	small, large := guidStoreHeld(t, acceptSmall), guidStoreHeld(t, acceptLarge)
	held := slope(small, large)
	bound := 1.5 * float64(guidMapRecSize)
	acceptance(t, 7, held <= bound, fmt.Sprintf("%.0f B a mailbox message held by the GUID store, bound %.0f", held, bound))
}

// writerAppendAlloc is what the writer's append of one message allocates in
// memory, on a folder of n held decoded, journal I/O aside.
func writerAppendAlloc(t *testing.T, n int) uint64 {
	t.Helper()
	home := movedFolder(t, n)
	h, f := acceptOpen(t, home)
	defer h.Close() //nolint:errcheck
	fs, err := h.(*userHandle).ui.state(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if err := fs.reload(); err != nil {
		t.Fatal(err)
	}
	const appends = 10
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	for i := uint32(0); i < appends; i++ {
		if _, err := fs.appendLockedWith(&mailbox.MessageMeta{UID: uint32(n) + 1 + i, Size: 10}, nil); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&m1)
	return (m1.TotalAlloc - m0.TotalAlloc) / appends
}

// The writer's append searches for its uid once, without allocating, and the
// replay it goes through searches no more: what it allocates does not grow
// with the folder (#2629 review).
func TestTheWritersAppendAllocatesNothingPerFolderMessage(t *testing.T) {
	small, large := writerAppendAlloc(t, acceptSmall), writerAppendAlloc(t, acceptLarge)
	if grow := slope(small, large); grow > changeGrowth {
		t.Errorf("one append allocates %d B at %d, %d B at %d: %.1f B a folder message, bound %d",
			small, acceptSmall, large, acceptLarge, grow, changeGrowth)
	}
}
