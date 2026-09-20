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
	mutLogWriteFails = func() error { return full }
	t.Cleanup(func() { mutLogWriteFails = nil })

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
		t.Errorf("log grew to %d from %d", st.Size(), wantLog.Size())
	}

	// The uid the refused transaction took must be free for the next one.
	mutLogWriteFails = nil
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
