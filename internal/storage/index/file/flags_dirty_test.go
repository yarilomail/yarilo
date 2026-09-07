package file

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The mark is what protects a record whose flags never reached storage, and the
// reader that acts on it is another process: it has to survive a reopen. Written
// to the base alone it does not — the log replays the older flags over it (#1724).
func TestTheDirtyMarkSurvivesAReopen(t *testing.T) {
	dir := t.TempDir()
	a := openIdx(dir, testUser)
	f, err := a.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Size: 10, Flags: []string{`\Seen`}}); err != nil {
		t.Fatal(err)
	}
	if err := a.UpdateFlags(f.ID, 1, []string{`\Seen`, `\Flagged`}, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.SetFlagsDirty(f.ID, 1, true); err != nil {
		t.Fatal(err)
	}
	a.Close() //nolint:errcheck

	b := openIdx(dir, testUser)
	defer b.Close() //nolint:errcheck
	fb, err := b.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := b.GetMessages(fb.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("the folder holds %d records, want 1", len(msgs))
	}
	if !msgs[0].FlagsDirty {
		t.Error("the record reads back clean; the mark did not survive the reopen")
	}
	if err := b.SetFlagsDirty(fb.ID, 1, false); err != nil {
		t.Fatal(err)
	}
	msgs, _ = b.GetMessages(fb.ID, mailbox.SeqSet{})
	if msgs[0].FlagsDirty {
		t.Error("the mark was cleared and the record still reads dirty")
	}
}
