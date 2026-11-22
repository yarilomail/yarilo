package file

import (
	"encoding/binary"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// keptFolder is a folder of four messages and the modseq a client's state
// would carry now, then uid 2 expunged after that state.
func keptFolder(t *testing.T, opts ...Option) (*userIndex, *mailbox.Folder, uint64, string) {
	t.Helper()
	home := t.TempDir()
	u := New(opts...).OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	f, err := u.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		uid, err := u.AllocateUID(f.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := u.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Size: 10, GUID: [16]byte{byte(uid), 0x2e}}); err != nil {
			t.Fatal(err)
		}
	}
	state := highestOf(t, u, f.ID)
	if err := u.ExpungeMessage(f.ID, 2); err != nil {
		t.Fatal(err)
	}
	return u, f, state, home
}

func highestOf(t *testing.T, u *userIndex, folderID uint64) uint64 {
	t.Helper()
	var m uint64
	if err := u.withFolderSite(folderID, lockSiteExpungeFloor, func(fs *folderState) error {
		var err error
		m, err = fs.highestModSeq()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return m
}

// answers says whether a state is still answered, as Email/changes and
// VANISHED each decide it under the lock they read with, and what vanished
// since; the two must agree.
func answers(t *testing.T, u *userIndex, folderID, state uint64) (bool, []uint32, []uint32) {
	t.Helper()
	copies, complete, err := u.VanishedGUIDs(folderID, state)
	if err != nil {
		t.Fatal(err)
	}
	var byGUID []uint32
	for _, c := range copies {
		byGUID = append(byGUID, c.UID)
	}
	vanished, reaches, err := u.Vanished(folderID, state)
	if err != nil {
		t.Fatal(err)
	}
	if complete != reaches {
		t.Fatalf("Email/changes decided %v and VANISHED %v for one state", complete, reaches)
	}
	if !reaches && (len(byGUID) != 0 || len(vanished) != 0) {
		t.Fatalf("a state past the history was answered with %v and %v", byGUID, vanished)
	}
	return reaches, byGUID, vanished
}

func indexPathOf(t *testing.T, u *userIndex, folderID uint64) string {
	t.Helper()
	fs, err := u.state(folderID)
	if err != nil {
		t.Fatal(err)
	}
	return fs.indexPath
}

// A state issued before one fold is still answered, and both readers of the
// history name the expunge the fold moved into .log.2; a second fold takes
// that history, and the same state is refused (#2664).
func TestAStateBeforeOneFoldIsAnsweredAndBeforeTwoIsNot(t *testing.T) {
	u, f, state, _ := keptFolder(t)
	if err := u.OptimizeIndex(f.ID); err != nil {
		t.Fatal(err)
	}
	ok, byGUID, vanished := answers(t, u, f.ID, state)
	if !ok || len(byGUID) != 1 || byGUID[0] != 2 || len(vanished) != 1 || vanished[0] != 2 {
		t.Fatalf("after one fold: answered %v, VanishedGUIDs %v, Vanished %v; want answered with uid 2 from both", ok, byGUID, vanished)
	}
	// A state between the folds: the log the first fold started is the history
	// the second keeps, from the start that fold wrote into it.
	between := highestOf(t, u, f.ID)
	if err := u.ExpungeMessage(f.ID, 3); err != nil {
		t.Fatal(err)
	}
	if err := u.OptimizeIndex(f.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := answers(t, u, f.ID, state); ok {
		t.Fatal("after two folds the state is still answered; its expunge is in no log any more")
	}
	if ok, _, vanished := answers(t, u, f.ID, between); !ok || len(vanished) != 1 || vanished[0] != 3 {
		t.Fatalf("a state between the folds: answered %v, Vanished %v; want answered with uid 3", ok, vanished)
	}
}

// A .log.2 of another lineage is a leftover of an earlier fold: its expunges
// are not the history the base's floor counts from, and it moves nothing.
func TestAKeptLogOfAnotherLineageIsIgnored(t *testing.T) {
	u, f, state, _ := keptFolder(t)
	if err := u.OptimizeIndex(f.ID); err != nil {
		t.Fatal(err)
	}
	p := indexPathOf(t, u, f.ID) + log2Suffix
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint32(raw[8:], binary.LittleEndian.Uint32(raw[8:])+7) // FileSeq
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, byGUID, _ := answers(t, u, f.ID, state); ok || len(byGUID) != 0 {
		t.Fatalf("a kept log of another lineage was read: answered %v, VanishedGUIDs %v", ok, byGUID)
	}
}

// A fold that stops after its base write and before the rename leaves the old
// log where the base names it as folded: another process reading in that
// window and after it sees every record once, and the floor is the fold point.
func TestAFoldStoppedBeforeTheRenameLeavesTheFolderWhole(t *testing.T) {
	u, f, state, home := keptFolder(t)
	other := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle)
	defer other.Close() //nolint:errcheck
	if _, err := other.OpenFolder("INBOX", 0); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		msgs, err := other.GetMessagesUnlocked(f.ID, mailbox.SeqSet{})
		if err != nil {
			t.Fatal(err)
		}
		return len(msgs)
	}
	var inWindow int
	renameLog = func(string, string) error {
		inWindow = count()
		return errors.New("stopped before the rename")
	}
	defer func() { renameLog = os.Rename }()
	if err := u.OptimizeIndex(f.ID); err == nil {
		t.Fatal("the fold reported success past a failed rename")
	}
	if inWindow != 3 || count() != 3 {
		t.Fatalf("the other process read %d records in the window and %d after, want 3 and 3", inWindow, count())
	}
	if _, err := os.Stat(indexPathOf(t, u, f.ID) + log2Suffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a kept log appeared from a fold that never renamed: %v", err)
	}
	if ok, _, _ := answers(t, u, f.ID, state); ok {
		t.Fatal("with no kept log the floor is the fold point; the state was answered anyway")
	}
}

// Reads, deliveries, flag changes and opens never open .log.2: only the
// history's readers do (#2664).
func TestReadsAndDeliveriesDoNotOpenTheKeptLog(t *testing.T) {
	u, f, state, _ := keptFolder(t)
	if err := u.OptimizeIndex(f.ID); err != nil {
		t.Fatal(err)
	}
	ResetLog2Opens()
	uid, err := u.AllocateUID(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Size: 10}); err != nil {
		t.Fatal(err)
	}
	if err := u.UpdateFlags(f.ID, uid, []string{`\Seen`}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := u.GetMessages(f.ID, mailbox.SeqSet{}); err != nil {
		t.Fatal(err)
	}
	if _, err := u.OpenFolder("INBOX", 0, ""); err != nil {
		t.Fatal(err)
	}
	if n := Log2Opens(); n != 0 {
		t.Fatalf("an ordinary read or write opened the kept log %d times", n)
	}
	answers(t, u, f.ID, state)
	if Log2Opens() == 0 {
		t.Fatal("the history's readers did not open the kept log either; the counter proves nothing")
	}
}

// A kept log past the age is removed under the folder lock, and a zero age
// keeps none: the folder then answers as before the change.
func TestTheKeptLogsAge(t *testing.T) {
	tests := []struct {
		name   string
		maxAge time.Duration
	}{{"past its age", time.Nanosecond}, {"none kept", 0}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, f, state, _ := keptFolder(t, WithLog2MaxAge(tc.maxAge))
			if err := u.OptimizeIndex(f.ID); err != nil {
				t.Fatal(err)
			}
			if ok, _, _ := answers(t, u, f.ID, state); ok {
				t.Fatal("a state before the fold was answered with no kept log in its age")
			}
			if _, err := os.Stat(indexPathOf(t, u, f.ID) + log2Suffix); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the kept log is still there: %v", err)
			}
		})
	}
}

// A rename carries the history with the folder, a delete takes it, and a
// rebuild drops it: its expunges are not the rebuilt folder's.
func TestTheKeptLogFollowsItsFolder(t *testing.T) {
	t.Run("rename", func(t *testing.T) {
		u, _, _, _ := keptFolder(t)
		g, err := u.OpenFolder("Before", 1, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := u.OptimizeIndex(g.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(indexPathOf(t, u, g.ID) + log2Suffix); err != nil {
			t.Fatalf("no kept log to carry: %v", err)
		}
		if err := u.RenameFolder("Before", "After"); err != nil {
			t.Fatal(err)
		}
		moved := indexPathFor(u.indexDir("After")) + log2Suffix
		if _, err := os.Stat(moved); err != nil {
			t.Fatalf("the kept log did not move with the folder: %v", err)
		}
	})
	t.Run("delete", func(t *testing.T) {
		u, f, _, _ := keptFolder(t)
		if _, err := u.OpenFolder("Gone", 1, ""); err != nil {
			t.Fatal(err)
		}
		g, err := u.OpenFolder("Gone", 0, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := u.OptimizeIndex(g.ID); err != nil {
			t.Fatal(err)
		}
		p := indexPathOf(t, u, g.ID) + log2Suffix
		if err := u.DeleteFolder("Gone"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the kept log outlived its folder: %v", err)
		}
		_ = f
	})
	t.Run("rebuild", func(t *testing.T) {
		u, f, _, _ := keptFolder(t)
		if err := u.OptimizeIndex(f.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := u.ResetFolder(f.ID, []*mailbox.MessageMeta{{UID: 1, Size: 10}}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(indexPathOf(t, u, f.ID) + log2Suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a rebuild kept the old history: %v", err)
		}
	})
}
