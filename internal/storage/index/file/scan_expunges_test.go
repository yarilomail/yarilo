package file

import (
	"io"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// VANISHED reads the journal in one call, not two per record: it runs on every
// QRESYNC reconnect, over a journal that reaches ~128 KB between folds (#1849).
func TestAVanishedScanReadsTheJournalOnce(t *testing.T) {
	root := t.TempDir()
	ui := openIdx(root, "u@x.com")
	f, err := ui.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatal(err)
	}
	const n = 30
	uids := make([]uint32, 0, n)
	for i := 0; i < n; i++ {
		m := &mailbox.MessageMeta{Size: 10, VSize: 10}
		if aerr := ui.AllocateAndAppend(f.ID, m); aerr != nil {
			t.Fatal(aerr)
		}
		uids = append(uids, m.UID)
	}
	for _, uid := range uids[:10] {
		if eerr := ui.ExpungeMessage(f.ID, uid); eerr != nil {
			t.Fatal(eerr)
		}
	}

	fs := ui.open[f.ID]
	counted := countJournalReads(t, fs.indexPath)
	defer counted.restore()

	gone, serr := scanExpungesSince(fs.indexPath, 0)
	if serr != nil {
		t.Fatal(serr)
	}
	if len(gone) != 10 {
		t.Fatalf("the scan found %d expunges, want 10", len(gone))
	}
	// Two reads per record would be dozens; the whole tail is one.
	if got := counted.n(); got > 4 {
		t.Errorf("the scan read the journal %d times, want a handful", got)
	}

	// The GUID half walks the same records and pays the same once.
	counted.reset()
	guids, gerr := scanExpungedGUIDsSince(fs.indexPath, 0)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if len(guids) != 10 {
		t.Fatalf("the GUID scan found %d expunges, want 10", len(guids))
	}
	if got := counted.n(); got > 4 {
		t.Errorf("the GUID scan read the journal %d times, want a handful", got)
	}
}

// countJournalReads counts every read of a journal opened from here on.
type journalCounter struct {
	reads   int
	restore func()
}

func (c *journalCounter) n() int { return c.reads }
func (c *journalCounter) reset() { c.reads = 0 }

func countJournalReads(t *testing.T, _ string) *journalCounter {
	t.Helper()
	c := &journalCounter{}
	prev := wrapLogReads
	wrapLogReads = func(ra io.ReaderAt) io.ReaderAt {
		return readerAtFunc(func(p []byte, off int64) (int, error) {
			c.reads++
			return ra.ReadAt(p, off)
		})
	}
	c.restore = func() { wrapLogReads = prev }
	return c
}

type readerAtFunc func([]byte, int64) (int, error)

func (f readerAtFunc) ReadAt(p []byte, off int64) (int, error) { return f(p, off) }
