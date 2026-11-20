package file

import (
	"bytes"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A header extension update journalled by a writer that has just folded
// reaches a reader: the intro names the extension by its place in the base
// the fold wrote, which lists them in layout order, not in the order the
// writer added them (#2643).
func TestAHeaderUpdateAfterAFoldReachesTheReader(t *testing.T) {
	home := movedFolder(t, 1000)
	wrong := 0
	for i := 1; i <= 200; i++ {
		w := New(WithLogCompaction(32<<10, 1<<20, 0)).OpenUser(&mailbox.UserInfo{Username: testUser, Home: home})
		f, err := w.OpenFolder("INBOX", 0)
		if err != nil {
			t.Fatal(err)
		}
		uid, err := w.AllocateUID(f.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Size: 10, GUID: [16]byte{byte(i), byte(i >> 8), 3}, InternalDate: time.Unix(1700000000, 0)}); err != nil {
			t.Fatal(err)
		}
		// A flag change moves the modseq extension's header: one more update
		// that names its extension by place.
		if err := w.UpdateFlags(f.ID, uid, []string{`\Seen`}, nil); err != nil {
			t.Fatal(err)
		}
		if err := w.(*userHandle).SetMaildirStamp(f.ID, mailbox.MaildirStamp{UIDListSize: uint32(i)}); err != nil {
			t.Fatal(err)
		}
		journalMisplaceable(t, w, f.ID)
		want := extHeaders(t, w, f.ID)
		w.Close() //nolint:errcheck
		r := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home})
		rf, err := r.OpenFolder("INBOX", 0)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := r.(*userHandle).MaildirStamp(rf.ID); got.UIDListSize != uint32(i) {
			wrong++
			t.Logf("after stamp %d a reader reads %d", i, got.UIDListSize)
		}
		// Every extension's header as the writer holds it: an update placed
		// at another extension's place shows in that extension's bytes.
		for name, b := range extHeaders(t, r, rf.ID) {
			if !bytes.Equal(b, want[name]) {
				wrong++
				t.Logf("after stamp %d the reader's %s header is %x, the writer's %x", i, name, b, want[name])
			}
		}
		r.Close() //nolint:errcheck
	}
	if wrong != 0 {
		t.Errorf("%d headers of 200 rounds differ between the writer and a reader", wrong)
	}
}

// journalMisplaceable journals, through the writer, an update of a header
// extension whose place in the writer's list holds, in the base on disk,
// another extension with room for its bytes: the same bytes it already has,
// so only a misplaced update shows (#2645).
func journalMisplaceable(t *testing.T, h mailbox.UserIndex, id uint64) {
	t.Helper()
	fs, err := h.(*userHandle).ui.state(id)
	if err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	base, err := mailindex.Open(fs.indexPath)
	if err != nil {
		t.Fatal(err)
	}
	for k, e := range fs.file.Extensions {
		if k >= len(base.Extensions) || len(e.HdrData) == 0 {
			continue
		}
		if b := base.Extensions[k]; b.Name != e.Name && b.Name != extNameHdrVsize && len(b.HdrData) >= len(e.HdrData) {
			intro, err := fs.extIntroRecord(e.Name)
			if err != nil {
				t.Fatal(err)
			}
			if err := fs.journalRecords([][]byte{intro, encLogRec(mailindex.TxTypeExtHdrUpdate, 0,
				mailindex.EncodeTxExtHdrUpdatePayload(mailindex.TxExtHdrUpdate{Data: append([]byte(nil), e.HdrData...)}))}); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
}

// extHeaders is every extension's header data the state holds, by name.
func extHeaders(t *testing.T, h mailbox.UserIndex, id uint64) map[string][]byte {
	t.Helper()
	fs, err := h.(*userHandle).ui.state(id)
	if err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if err := fs.reload(); err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, e := range fs.file.Extensions {
		// The aggregate is the one header a writer and a reader may count
		// differently between catch-ups.
		if e.Name != extNameHdrVsize {
			out[e.Name] = append([]byte(nil), e.HdrData...)
		}
	}
	return out
}
