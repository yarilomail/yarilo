package file

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// logged runs f with the default logger captured and returns what it wrote.
func logged(t *testing.T, f func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	f()
	return buf.String()
}

func headerOf(t *testing.T, u *userIndex, fid uint64) mailindex.Header {
	t.Helper()
	fs, err := u.state(fid)
	if err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.file.Header
}

// A writer whose chunks were not loaded delivers, marks \Deleted and
// expunges: loading the chunks for the expunge applies its deferred records
// once, its counters stay the journal's, and nothing reads as drift. Each
// record moved the counters twice before, and the check corrected them back
// (2.4.8 window, 2363 corrections, messages 399 on 200 records).
func TestDeferredRecordsAppliedAtLoadAreNoDrift(t *testing.T) {
	const n = 3 * mailindex.ChunkRecords
	home := pendingFolder(t, n)
	out := logged(t, func() {
		w, wf := acceptOpen(t, home)
		for range 5 {
			uid, err := w.AllocateUID(wf.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.AppendMessage(wf.ID, &mailbox.MessageMeta{UID: uid, Size: 10}); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.UpdateFlags(wf.ID, 5, []string{`\Seen`, `\Deleted`}, nil); err != nil {
			t.Fatal(err)
		}
		if err := w.ExpungeMessage(wf.ID, 5); err != nil {
			t.Fatal(err)
		}
		h := headerOf(t, w.(*userHandle).ui, wf.ID)
		if h.MessagesCount != n+4 || h.DeletedMessagesCount != 0 {
			t.Errorf("writer header messages=%d deleted=%d, want %d/0", h.MessagesCount, h.DeletedMessagesCount, n+4)
		}
		w.Close() //nolint:errcheck
		if f := coldStatus(t, home); f.Messages != n+4 {
			t.Errorf("cold STATUS MESSAGES %d, want %d", f.Messages, n+4)
		}
	})
	if strings.Contains(out, "header corrected") || strings.Contains(out, "below zero") {
		t.Errorf("deferred records read as drift:\n%s", out)
	}
}

// A header that counts no \Deleted while a loaded record carries it: the
// expunge of that record recounts first and journals a zero, never a wrapped
// count, and the drift is logged once.
func TestAnExpungeOnACounterAtZeroRecountsFirst(t *testing.T) {
	dir := t.TempDir()
	seedBase(t, dir, [][]string{nil, {`\Deleted`}, nil})
	u := openIdx(dir, testUser)
	defer u.Close() //nolint:errcheck
	f, err := u.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	fs, err := u.state(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	var recs [][]byte
	out := logged(t, func() {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		if err := fs.loadWriterRecords(); err != nil {
			t.Fatal(err)
		}
		fs.file.Header.DeletedMessagesCount = 0
		if recs, err = fs.expungeLocked(2, 99); err != nil {
			t.Fatal(err)
		}
	})
	if !bytes.Contains(bytes.Join(recs, nil), encU32Update(44, 0)) {
		t.Error("the expunge did not journal deleted=0")
	}
	if h := headerOf(t, u, f.ID); h.DeletedMessagesCount != 0 || h.MessagesCount != 2 {
		t.Errorf("header messages=%d deleted=%d, want 2/0", h.MessagesCount, h.DeletedMessagesCount)
	}
	if got := strings.Count(out, "header corrected"); got != 1 || !strings.Contains(out, "field=deleted was=0 now=1") {
		t.Errorf("want one correction deleted 0->1, got %d:\n%s", got, out)
	}
}

// A journal expunge of one of two \Deleted records replayed on a header that
// counts none: the counter stays at zero, the replay says so, and the recount
// brings it to the one \Deleted record left.
func TestAReplayedExpungeNeverWrapsACounter(t *testing.T) {
	dir := t.TempDir()
	base := seedBase(t, dir, [][]string{nil, {`\Deleted`}, {`\Deleted`}})
	rewriteBaseHeader(t, base, func(h *mailindex.Header) { h.DeletedMessagesCount = 0 })
	logWithout(t, dir, isHeaderUpdate, func(t *testing.T, fs *folderState) [][]byte {
		return [][]byte{encLogRec(mailindex.TxTypeExpungeGUID, mailindex.TxExpungeProt, make28(2))}
	})
	var h mailindex.Header
	out := logged(t, func() {
		u := openIdx(dir, testUser)
		defer u.Close() //nolint:errcheck
		f, err := u.OpenFolder("INBOX", 0, "")
		if err != nil {
			t.Fatal(err)
		}
		h = headerOf(t, u, f.ID)
	})
	if h.DeletedMessagesCount != 1 || h.MessagesCount != 2 {
		t.Errorf("header messages=%d deleted=%d, want 2/1", h.MessagesCount, h.DeletedMessagesCount)
	}
	if !strings.Contains(out, "below zero") || !strings.Contains(out, "field=deleted") {
		t.Errorf("the replay did not say the counter was wrong:\n%s", out)
	}
}

func make28(uid uint32) []byte {
	p := make([]byte, 28)
	p[0], p[1], p[2], p[3] = byte(uid), byte(uid>>8), byte(uid>>16), byte(uid>>24)
	return p
}
