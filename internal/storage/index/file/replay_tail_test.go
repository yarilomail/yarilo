package file

import (
	"os"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A replay reads the tail in one call, not two per record: a folder's journal
// reaches ~128 KB between folds, and eight-byte reads make that 16k syscalls
// (#1846).
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

	logPath := ui.open[f.ID].indexPath + ".log"
	st, serr := os.Stat(logPath)
	if serr != nil || st.Size() < 400 {
		t.Fatalf("the journal is %v bytes, so this measures nothing", st)
	}

	// A fresh reader replays the whole journal: one ReadAt for the tail, and
	// the boundary probe only where a group runs past what it read.
	reads := 0
	restore := onLogRead(func() { reads++ })
	defer restore()

	fresh := openIdx(root, "u@x.com")
	f2, oerr := fresh.OpenFolder("INBOX", 42, "")
	if oerr != nil {
		t.Fatal(oerr)
	}
	msgs, gerr := fresh.GetMessages(f2.ID, mailbox.SeqSet{})
	if gerr != nil {
		t.Fatal(gerr)
	}
	if len(msgs) != 40 {
		t.Fatalf("the replay produced %d messages, want 40", len(msgs))
	}
	// Two per record would be 160 for this journal; the tail is one call per
	// pass, and a pass happens on open and on the read.
	if reads > 8 {
		t.Errorf("the replay made %d reads of the journal, want a handful", reads)
	}
}
