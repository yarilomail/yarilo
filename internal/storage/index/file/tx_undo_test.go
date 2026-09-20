package file

import (
	"errors"
	"os"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A commit whose log write fails must leave the folder as it was: the reference
// rolls back an in-memory transaction that never reached the file, and ours
// applies before it writes (#1831). Without the restore the next append skips a
// uid, the counts carry a message nothing recorded, and the flag change is
// visible to the session that never got it.
func TestCommitRestoresStateWhenLogRefuses(t *testing.T) {
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
	wantNextUID := fs.file.Header.NextUID
	wantCount := fs.file.Header.MessagesCount
	wantFlags := fs.file.Records[0].Flags
	wantLog, err := os.Stat(fs.indexPath + ".log")
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}

	full := errors.New("no space left on device")
	// A short write, not a refused one: the disk fills mid-group, and the
	// stump the BOUNDARY promises whole is what a reader would replay.
	mutLogWrite = func(f *os.File, buf []byte) (int, error) {
		n, err := f.Write(buf[:len(buf)/2])
		if err != nil {
			return n, err
		}
		return n, full
	}
	t.Cleanup(restoreMutLogSeams)

	tx, err := b.Begin(f.ID)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	added := &mailbox.MessageMeta{Size: 20}
	tx.Append(added)
	tx.UpdateFlags(uid, mailbox.FlagsUpdate{Mode: mailbox.FlagsAdd, Flags: []string{"\\Seen"}})
	if _, err := tx.Commit(); !errors.Is(err, full) {
		t.Fatalf("Commit err = %v, want the log's own error", err)
	}

	if got := fs.file.Header.NextUID; got != wantNextUID {
		t.Errorf("NextUID = %d after a refused log write, want %d", got, wantNextUID)
	}
	if got := fs.file.Header.MessagesCount; got != wantCount {
		t.Errorf("MessagesCount = %d, want %d", got, wantCount)
	}
	if got := len(fs.file.Records); got != int(wantCount) {
		t.Errorf("records = %d, want %d", got, wantCount)
	}
	if got := fs.file.Records[0].Flags; got != wantFlags {
		t.Errorf("flags = %#x, want %#x: a flag nothing logged stayed in memory", got, wantFlags)
	}
	if added.UID != 0 {
		t.Errorf("the caller's message carries uid %d that no log records", added.UID)
	}
	st, err := os.Stat(fs.indexPath + ".log")
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if st.Size() != wantLog.Size() {
		t.Errorf("log is %d bytes, want %d: the partial group stayed in the file", st.Size(), wantLog.Size())
	}

	// The log still parses to its end: a stump left behind is only visible to
	// a reader that replays it.
	restoreMutLogSeams()
	reader := openIdx(dir, testUser)
	rf, err := reader.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatalf("reopen after the refusal: %v", err)
	}
	msgs, err := reader.GetMessages(rf.ID, nil)
	if err != nil {
		t.Fatalf("GetMessages after the refusal: %v", err)
	}
	if len(msgs) != int(wantCount) {
		t.Errorf("a fresh reader sees %d messages, want %d", len(msgs), wantCount)
	}

	// The uid the refused transaction took must be free for the next one.
	tx2, err := b.Begin(f.ID)
	if err != nil {
		t.Fatalf("Begin again: %v", err)
	}
	again := &mailbox.MessageMeta{Size: 20}
	tx2.Append(again)
	if _, err := tx2.Commit(); err != nil {
		t.Fatalf("Commit after the refusal: %v", err)
	}
	if again.UID != wantNextUID {
		t.Errorf("the next append took uid %d, want %d: the refused transaction burned one", again.UID, wantNextUID)
	}
}

// restoreMutLogSeams puts the log's write and sync back to the real ones.
func restoreMutLogSeams() {
	mutLogWrite = func(f *os.File, buf []byte) (int, error) { return f.Write(buf) }
	mutLogSync = func(f *os.File) error { return f.Sync() }
}

// A sync that fails leaves the whole group in the file, and memory was rolled
// back: the log has to give it up too, or the next reader replays a
// transaction no session was told about (#1831).
func TestCommitTakesBackTheGroupWhenSyncFails(t *testing.T) {
	dir := t.TempDir()
	b := openIdx(dir, testUser)
	b.b.fsync = mailbox.FsyncAlways
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
	wantNextUID := fs.file.Header.NextUID
	wantLog, err := os.Stat(fs.indexPath + ".log")
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}

	ioerr := errors.New("input/output error")
	synced := false
	mutLogSync = func(f *os.File) error { synced = true; return ioerr }
	t.Cleanup(restoreMutLogSeams)

	tx, err := b.Begin(f.ID)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	tx.Append(&mailbox.MessageMeta{Size: 20})
	if _, err := tx.Commit(); !errors.Is(err, ioerr) {
		t.Fatalf("Commit err = %v, want the sync's own error", err)
	}
	if !synced {
		t.Fatal("the sync seam never ran: this folder does not make its journal durable, so the row proves nothing")
	}
	st, err := os.Stat(fs.indexPath + ".log")
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if st.Size() != wantLog.Size() {
		t.Errorf("log is %d bytes, want %d: a group nobody was told about stayed", st.Size(), wantLog.Size())
	}
	if got := fs.file.Header.NextUID; got != wantNextUID {
		t.Errorf("NextUID = %d, want %d", got, wantNextUID)
	}
}
