package file

import (
	"os"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/filelock"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// stumped is a folder whose log carries a complete history and, past it, the
// bytes of a group that never finished -- what a writer mid-append looks like
// to anyone reading the file.
func stumped(t *testing.T) (*folderState, string, int64) {
	t.Helper()
	dir := t.TempDir()
	b := openIdx(dir, testUser)
	f, err := b.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	uid, err := b.AllocateUID(f.ID)
	if err != nil {
		t.Fatalf("AllocateUID: %v", err)
	}
	if err := b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Size: 10}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	fs := b.open[f.ID]
	logPath := fs.indexPath + ".log"
	st, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	lf, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	if _, err := lf.Write([]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02}); err != nil {
		t.Fatalf("write the stump: %v", err)
	}
	if err := lf.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return fs, logPath, st.Size()
}

// replay is a full replay, as a reader's first open makes it.
func replay(t *testing.T, fs *folderState) {
	t.Helper()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var err error
	if fs.file, err = mailindex.Open(fs.indexPath); err != nil {
		t.Fatalf("reopen base: %v", err)
	}
	if _, err := fs.applyLog(0); err != nil {
		t.Fatalf("applyLog: %v", err)
	}
}

// A reader that cuts a partial tail takes the bytes of a writer that is still
// writing them. The reference declares a short tail only while it holds the
// log; ours truncated from any full replay, held or not (#1831).
func TestAStumpIsNotCutWhileTheJournalIsHeld(t *testing.T) {
	fs, logPath, clean := stumped(t)
	stumpSize := clean + 6

	// Someone else holds the journal: on this stand it is the same lock a
	// writer takes for its one write(2).
	h, err := filelock.Take(fs.indexPath+".lock", fs.lockMethod, time.Second)
	if err != nil {
		t.Fatalf("hold the journal: %v", err)
	}

	replay(t, fs)

	st, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if st.Size() != stumpSize {
		t.Errorf("the log is %d bytes, want %d: a reader cut a tail a writer may still be writing", st.Size(), stumpSize)
	}
	if rerr := h.Release(); rerr != nil {
		t.Fatalf("release: %v", rerr)
	}

	// With the journal free, the same replay does cut it: this is about who
	// may cut, not about leaving stumps behind for ever.
	replay(t, fs)
	st, err = os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if st.Size() != clean {
		t.Errorf("the log is %d bytes, want %d: the stump outlived a replay that held the journal", st.Size(), clean)
	}
}

// The stump completed between the read and the hold: those bytes are a whole
// group now, and cutting them back would lose a committed transaction.
func TestAStumpThatGrewUnderTheHoldIsLeftAlone(t *testing.T) {
	fs, logPath, clean := stumped(t)

	grown := false
	stumpGrew = func() {
		if grown {
			return
		}
		grown = true
		lf, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Errorf("open log: %v", err)
			return
		}
		defer lf.Close() //nolint:errcheck
		if _, werr := lf.Write([]byte{0x03, 0x04}); werr != nil {
			t.Errorf("finish the group: %v", werr)
		}
	}
	t.Cleanup(func() { stumpGrew = func() {} })

	replay(t, fs)

	if !grown {
		t.Fatal("the seam never fired, so nothing grew under the hold")
	}
	st, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if st.Size() == clean {
		t.Errorf("the log was cut back to %d: what arrived under the hold may be a complete group", clean)
	}
}
