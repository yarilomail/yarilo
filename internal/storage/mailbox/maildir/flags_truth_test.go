package maildir

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The name carries the flags, and the name is the truth: a file back at ":2,"
// takes \Seen off the record, where we used to keep the index's answer (#1700).
func TestTheNameTakesTheSeenFlagOffTheRecord(t *testing.T) {
	box, idx, folder := recSetup(t)
	name, _, _, err := box.Save("INBOX", strings.NewReader("body\n"), 0, 5, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	recAppend(t, box, idx, folder, &mailbox.MessageMeta{
		UID: 1, Filename: name, Size: 5, VSize: 5, Flags: []string{`\Seen`},
	})

	if _, err := box.ReconcileIndex(idx, folder); err != nil {
		t.Fatal(err)
	}
	msgs, _ := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if len(msgs) != 1 {
		t.Fatalf("record churned: %+v", msgs)
	}
	for _, f := range msgs[0].Flags {
		if f == `\Seen` {
			t.Errorf("the record still says %s, and the file's name says nothing", f)
		}
	}
}

// Unless the flags never reached the disk: a rename that failed marks the
// record dirty, and a sync leaves it alone until the write lands (#1700).
func TestADirtyRecordKeepsItsFlagsUntilTheRenameLands(t *testing.T) {
	box, idx, folder := recSetup(t)
	name, _, _, err := box.Save("INBOX", strings.NewReader("body\n"), 0, 5, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	recAppend(t, box, idx, folder, &mailbox.MessageMeta{
		UID: 1, Filename: name, Size: 5, VSize: 5, Flags: []string{`\Seen`},
	})
	marker, ok := idx.(mailbox.FlagsDirtyMarker)
	if !ok {
		t.Fatal("the index cannot say a record's flags are not on disk")
	}
	if err := marker.SetFlagsDirty(folder.ID, 1, true); err != nil {
		t.Fatal(err)
	}

	if _, err := box.ReconcileIndex(idx, folder); err != nil {
		t.Fatal(err)
	}
	msgs, _ := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if !hasFlagIn(msgs[0].Flags, `\Seen`) {
		t.Errorf("the flag the client was told about was taken off by a sync: %v", msgs[0].Flags)
	}

	// The write lands: the name carries it, the bit is cleared, and the two agree.
	stored := storedName(t, box, "INBOX", msgs[0])
	if _, err := box.WriteFlags("INBOX", stored, []string{`\Seen`}, nil); err != nil {
		t.Fatal(err)
	}
	if err := marker.SetFlagsDirty(folder.ID, 1, false); err != nil {
		t.Fatal(err)
	}
	if _, err := box.ReconcileIndex(idx, folder); err != nil {
		t.Fatal(err)
	}
	msgs, _ = idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if !hasFlagIn(msgs[0].Flags, `\Seen`) {
		t.Errorf("after the rename landed the record lost the flag: %v", msgs[0].Flags)
	}
}

// A record the list does not name stands for nothing a reader can open. It is
// left in place -- the file may be there under a name nobody recorded -- and
// said out loud once (#1693, #1700).
func TestARecordTheListDoesNotNameIsLeftAloneAndReported(t *testing.T) {
	box, idx, folder := recSetup(t)
	name, _, _, err := box.Save("INBOX", strings.NewReader("body\n"), 0, 5, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	// Recorded in the index and nowhere else: no list entry for this uid.
	if err := idx.AppendMessage(folder.ID, &mailbox.MessageMeta{
		UID: 1, Filename: name, Size: 5, VSize: 5,
	}); err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelError})))
	defer slog.SetDefault(prev)

	for i := 0; i < 2; i++ {
		if _, err := box.ReconcileIndex(idx, folder); err != nil {
			t.Fatal(err)
		}
	}
	msgs, _ := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	found := false
	for _, m := range msgs {
		if m.UID == 1 {
			found = true
		}
	}
	if !found {
		t.Error("the record was expunged; its file may be on disk under a name nobody recorded")
	}
	if got := strings.Count(logged.String(), "names no file for this record"); got != 1 {
		t.Errorf("the record was reported %d times over two passes, want once", got)
	}
}
