package file

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// writerAfterViewHome, set, makes this test the other process.
const writerAfterViewHome = "YARILO_WRITER_AFTER_VIEW_HOME"

// otherProcessStep is what the other process does to the folder: three
// deliveries stamped as a maildir save stamps them, then one change.
func otherProcessStep(t *testing.T, home, step string) {
	info := &mailbox.UserInfo{Username: testUser, Home: home}
	a := New().OpenUser(info).(*userHandle)
	defer a.Close() //nolint:errcheck
	f, err := a.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	switch step {
	case "setup":
		for i := 0; i < 3; i++ {
			m := &mailbox.MessageMeta{Size: 10, GUID: [16]byte{byte(i + 1), 0x77}}
			if err := a.AllocateAndAppendStamped(f.ID, m, func(uid uint32) (string, *mailbox.MaildirStamp, error) {
				return "", &mailbox.MaildirStamp{UIDListSize: 100 * uid}, nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	case "expunge":
		err = a.ExpungeMessage(f.ID, 2)
	case "flags":
		err = a.UpdateFlags(f.ID, 2, []string{`\Seen`}, nil)
	case "append":
		err = a.AllocateAndAppend(f.ID, &mailbox.MessageMeta{Size: 10, GUID: [16]byte{4, 0x77}})
	}
	if err != nil {
		t.Fatal(err)
	}
}

// A writer whose chunks another reader of the shared base loaded applies what
// it deferred first, in journal order: replayed after the change it saw since,
// a deferred append undid another process's expunge (#2663). The other
// process is a real one, so no memory is shared with it.
func TestAWriterLoadedAfterAViewMissesNoChange(t *testing.T) {
	if home := os.Getenv(writerAfterViewHome); home != "" {
		otherProcessStep(t, home, os.Getenv(writerAfterViewHome+"_STEP"))
		return
	}
	tests := []struct {
		step  string
		check func(t *testing.T, fs *folderState)
	}{
		{"expunge", func(t *testing.T, fs *folderState) {
			if taken, err := fs.uidTaken(2); err != nil || taken || fs.recordCount() != 2 {
				t.Errorf("the writer holds %d records, uid 2 taken %v (%v); want the expunge applied", fs.recordCount(), taken, err)
			}
		}},
		{"flags", func(t *testing.T, fs *folderState) {
			seq, ok := fs.recSeqOfUID(2)
			if !ok || fs.recFlags(seq)&mailindex.FlagSeen == 0 {
				t.Errorf("the writer reads uid 2 without the other process's \\Seen")
			}
		}},
		{"append", func(t *testing.T, fs *folderState) {
			if taken, err := fs.uidTaken(4); err != nil || !taken || fs.recordCount() != 4 {
				t.Errorf("the writer holds %d records, uid 4 taken %v (%v); want the other process's append", fs.recordCount(), taken, err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.step, func(t *testing.T) {
			home := testHome(t.TempDir(), testUser)
			other := func(step string) {
				c := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAWriterLoadedAfterAViewMissesNoChange$", "-test.count=1")
				c.Env = append(os.Environ(), writerAfterViewHome+"="+home, writerAfterViewHome+"_STEP="+step)
				if out, err := c.CombinedOutput(); err != nil {
					t.Fatalf("the other process, %s: %v\n%s", step, err, out)
				}
			}
			other("setup")
			b := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle)
			defer b.Close() //nolint:errcheck
			f, err := b.OpenFolder("INBOX", 0)
			if err != nil {
				t.Fatal(err)
			}
			other(tc.step)
			if _, err := b.GetMessages(f.ID, mailbox.SeqSet{}); err != nil {
				t.Fatal(err)
			}
			if err := b.ui.withFolderSite(f.ID, lockSiteAppend, func(fs *folderState) error {
				tc.check(t, fs)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if tc.step == "expunge" {
				if err := b.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 2, Size: 10}); !errors.Is(err, mailbox.ErrUIDNotIssued) {
					t.Errorf("an append under the expunged uid 2: %v, want refused as given up", err)
				}
			}
		})
	}
}
