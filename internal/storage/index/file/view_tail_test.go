package file

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// tailReadAlloc is what a session's open of a folder of n, past a journal
// tail another session wrote (three deliveries and \Seen taken from an
// old message), allocates and loads, and what it answers against the writer.
func tailReadAlloc(t *testing.T, n int) (alloc uint64, loads int64, got, want *mailbox.Folder) {
	t.Helper()
	home := movedFolder(t, n)
	r, _ := acceptOpen(t, home)
	defer r.Close() //nolint:errcheck
	w, wf := acceptOpen(t, home)
	defer w.Close() //nolint:errcheck
	for i := range 3 {
		uid, err := w.AllocateUID(wf.ID)
		if err != nil {
			t.Fatal(err)
		}
		m := &mailbox.MessageMeta{UID: uid, GUID: [16]byte{byte(i + 1)}, Size: 1200, VSize: 1230, InternalDate: time.Unix(1700000000, 0)}
		if err := w.AppendMessage(wf.ID, m); err != nil {
			t.Fatal(err)
		}
	}
	// Takes \Seen away from a message of the first chunk.
	if err := w.UpdateFlags(wf.ID, 5, []string{`\Flagged`}, nil); err != nil {
		t.Fatal(err)
	}
	want, err := w.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	// The base as no session has read it: no chunk in memory.
	baseImages.Delete(indexPathFor(home))
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	opens := mailindex.LazyOpens()
	got, err = r.OpenFolder("INBOX", 0)
	loads = mailindex.LazyOpens() - opens
	runtime.ReadMemStats(&m1)
	if err != nil {
		t.Fatal(err)
	}
	// What the open deferred, a read of the records then shows.
	msgs, err := r.GetMessages(wf.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != n+3 || flagged(msgs[4].Flags, `\Seen`) || !flagged(msgs[4].Flags, `\Flagged`) {
		t.Fatalf("%d: the records read after the open: %d, message 5 flags %v", n, len(msgs), msgs[4].Flags)
	}
	return m1.TotalAlloc - m0.TotalAlloc, loads, got, want
}

// A reader opening a folder past a tail that touches a few chunks loads none
// of them, answers as the writer does, and allocates nothing a folder message.
func TestAReaderPastATailLoadsNoChunk(t *testing.T) {
	var allocs [2]uint64
	for i, n := range []int{acceptSmall, acceptLarge} {
		alloc, loads, got, want := tailReadAlloc(t, n)
		allocs[i] = alloc
		if loads != 0 {
			t.Errorf("%d: the open past the tail read the base %d times", n, loads)
		}
		if got.Messages != want.Messages || got.Unseen != want.Unseen || got.NextUID != want.NextUID || got.Recent != want.Recent {
			t.Errorf("%d: the reader answers messages %d unseen %d next %d recent %d, the writer %d %d %d %d",
				n, got.Messages, got.Unseen, got.NextUID, got.Recent, want.Messages, want.Unseen, want.NextUID, want.Recent)
		}
	}
	grow := slope(allocs[0], allocs[1])
	if grow > acceptNoise {
		t.Errorf("%s", fmt.Sprintf("the open past the tail allocates %d B at %d, %d B at %d: %.1f B a message, bound %d",
			allocs[0], acceptSmall, allocs[1], acceptLarge, grow, acceptNoise))
	}
}

func flagged(flags []string, f string) bool {
	for _, x := range flags {
		if x == f {
			return true
		}
	}
	return false
}
