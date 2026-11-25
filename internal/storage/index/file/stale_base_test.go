package file

import (
	"os"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A writer whose stat of the base still matches after another process folded
// it, as a reused inode with the same size and mtime looks on ext4: the
// reload's fast path keeps the old state. The writer is held to the base on
// disk by its lineage and digest, reads again, and journals the counters of
// the base that is there, not of the one it held (#2591).
func TestAWriterHeldToAnOldBaseReadsAgainBeforeItWrites(t *testing.T) {
	home := t.TempDir()
	w := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	t.Cleanup(func() { _ = w.Close() })
	f, err := w.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 20; i++ {
		if err := w.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uint32(i), Size: 10}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.OptimizeIndex(f.ID); err != nil {
		t.Fatal(err)
	}
	fs, err := w.state(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Another process: \Seen on 3, expunge 5, fold.
	foldFromAnotherProcess(t, home, "3:5")

	// The file now looks like the one this state took: same inode, size and
	// mtime, and the log no longer than it was.
	fs.mu.Lock()
	st, err := os.Stat(fs.indexPath)
	if err != nil {
		t.Fatal(err)
	}
	fs.baseIdent, fs.baseMod = st, st.ModTime()
	fs.closeFDs()
	if lst, err := os.Stat(fs.indexPath + ".log"); err == nil {
		fs.logSize = lst.Size()
	}
	fs.mu.Unlock()

	before := testutil.ToFloat64(metricStaleBase.WithLabelValues("write-flags", "flush", "base", "yes"))
	if err := w.UpdateFlags(f.ID, 7, []string{`\Seen`}, nil); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	h, n := fs.file.Header, fs.recordCount()
	fs.mu.Unlock()
	if n != 19 || h.MessagesCount != 19 || h.SeenMessagesCount != 2 {
		t.Errorf("after the write: %d records, header messages=%d seen=%d, want 19/19/2", n, h.MessagesCount, h.SeenMessagesCount)
	}
	if got := testutil.ToFloat64(metricStaleBase.WithLabelValues("write-flags", "flush", "base", "yes")) - before; got != 1 {
		t.Errorf("stale base counted %v under write-flags/flush/base/yes, want 1", got)
	}
}

// A writer's header its own loaded records disagree with is taken from the
// records before the write journals it, counted under check=counters.
func TestAWritersHeaderIsHeldToItsRecordsBeforeItWrites(t *testing.T) {
	home := t.TempDir()
	w := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	t.Cleanup(func() { _ = w.Close() })
	f, err := w.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		if err := w.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uint32(i), Size: 10, Flags: []string{`\Deleted`}}); err != nil {
			t.Fatal(err)
		}
	}
	fs, err := w.state(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	if err := fs.loadWriterRecords(); err != nil {
		t.Fatal(err)
	}
	fs.file.Header.DeletedMessagesCount = 0
	from := fs.baseFrom
	fs.mu.Unlock()
	before := testutil.ToFloat64(metricStaleBase.WithLabelValues("write-flags", from, "counters", "yes"))
	if err := w.UpdateFlags(f.ID, 2, []string{`\Seen`, `\Deleted`}, nil); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	d := fs.file.Header.DeletedMessagesCount
	fs.mu.Unlock()
	if d != 5 {
		t.Errorf("deleted=%d after the write, want 5", d)
	}
	if got := testutil.ToFloat64(metricStaleBase.WithLabelValues("write-flags", from, "counters", "yes")) - before; got != 1 {
		t.Errorf("counted %v under write-flags/%s/counters/yes, want 1", got, from)
	}
}
