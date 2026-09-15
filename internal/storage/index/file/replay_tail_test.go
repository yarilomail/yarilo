package file

import (
	"io"
	"os"
	"sync/atomic"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A replay reads the tail in one call, not two per record: a 128 KB journal
// makes that 16k syscalls (#1846).
func TestAReplayReadsTheTailInOneCall(t *testing.T) {
	root := t.TempDir()
	ui := openIdx(root, "u@x.com")
	f, err := ui.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if aerr := ui.AllocateAndAppend(f.ID, &mailbox.MessageMeta{Size: 10, VSize: 10}); aerr != nil {
			t.Fatal(aerr)
		}
	}
	fs := ui.open[f.ID]
	st, serr := os.Stat(fs.indexPath + ".log")
	if serr != nil || st.Size() < 400 {
		t.Fatalf("the journal is %v, so this measures nothing", st)
	}

	// Counted on the descriptor the replay reads through, so the tail, the
	// boundary probe and any catch-up are all counted.
	lg, lerr := openLogRead(fs.indexPath)
	if lerr != nil {
		t.Fatal(lerr)
	}
	defer lg.close()
	counted := &countingAt{ra: lg.ra}
	lg.ra = counted

	view := &folderState{
		user: fs.user, folder: fs.folder, indexDir: fs.indexDir, indexPath: fs.indexPath,
		file: cloneIndexFile(fs.file), lineage: fs.lineage,
	}
	view.file.Records = nil
	view.file.Header.MessagesCount = 0
	if _, aerr := view.applyLogFrom(lg, int64(0)); aerr != nil {
		t.Fatal(aerr)
	}
	if got := len(view.file.Records); got != 40 {
		t.Fatalf("the replay produced %d records, want 40", got)
	}
	// Two reads per record would be 80 for this journal; the whole tail is one.
	if got := counted.n.Load(); got > 4 {
		t.Errorf("the replay read the journal %d times, want a handful", got)
	}
}

// countingAt counts every read of the journal: the tail, the probe and the
// catch-up all go through this descriptor (#1846).
type countingAt struct {
	ra io.ReaderAt
	n  atomic.Int64
}

func (c *countingAt) ReadAt(p []byte, off int64) (int, error) {
	c.n.Add(1)
	return c.ra.ReadAt(p, off)
}

// A group that arrives between the reader's stat and its replay is applied,
// not stranded behind a committedEnd that says it was (#1833, #1846).
func TestAGroupWrittenAfterTheStatIsStillApplied(t *testing.T) {
	root := t.TempDir()
	ui := openIdx(root, "u@x.com")
	f, err := ui.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatal(err)
	}
	if aerr := ui.AllocateAndAppend(f.ID, &mailbox.MessageMeta{Size: 10, VSize: 10}); aerr != nil {
		t.Fatal(aerr)
	}
	fs := ui.open[f.ID]
	logPath := fs.indexPath + ".log"

	// A reader opens the log, then a writer appends a whole group: the reader's
	// size is now short of what the file holds.
	lg, lerr := openLogRead(fs.indexPath)
	if lerr != nil {
		t.Fatal(lerr)
	}
	defer lg.close()
	shortSize := lg.size
	if aerr := ui.AllocateAndAppend(f.ID, &mailbox.MessageMeta{Size: 10, VSize: 10}); aerr != nil {
		t.Fatal(aerr)
	}
	st, serr := os.Stat(logPath)
	if serr != nil || st.Size() <= shortSize {
		t.Fatalf("the second append did not reach the log: %v", st)
	}

	// The size caught the BOUNDARY and nothing after it: the case that strands
	// records, because the probe then calls the group whole.
	lg.size = shortSize + 12

	view := &folderState{
		user: fs.user, folder: fs.folder, indexDir: fs.indexDir, indexPath: fs.indexPath,
		file: cloneIndexFile(fs.file), lineage: fs.lineage,
	}
	view.file.Records = nil
	view.file.Header.MessagesCount = 0
	end, aerr := view.applyLogFrom(lg, int64(0))
	if aerr != nil {
		t.Fatal(aerr)
	}
	if end <= lg.size {
		t.Fatalf("the pass confirmed %d bytes, no further than the boundary it saw (%d)", end, lg.size)
	}
	if got := len(view.file.Records); got != 2 {
		t.Errorf("the replay applied %d records, want 2: a group the probe called whole was left behind", got)
	}
}
