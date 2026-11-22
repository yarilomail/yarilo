package file

import (
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// Another process reads the folder between a fold's base write and its log
// replacement, then the writer appends on: every later read of that process
// sees every record (#2660's lesson for the folder index).
func TestAReadInsideAFoldSeesTheRecordsAfterIt(t *testing.T) {
	root := t.TempDir()
	home := testHome(root, testUser)
	open := func() *userHandle {
		return New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle)
	}
	a, b := open(), open()
	defer a.Close() //nolint:errcheck
	defer b.Close() //nolint:errcheck
	f, err := a.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	add := func() uint32 {
		uid, err := a.AllocateUID(f.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Size: 10}); err != nil {
			t.Fatal(err)
		}
		return uid
	}
	for i := 0; i < 20; i++ {
		add()
	}
	if _, err := b.OpenFolder("INBOX", 0); err != nil {
		t.Fatal(err)
	}
	read := func() map[uint32]bool {
		msgs, err := b.GetMessagesUnlocked(f.ID, mailbox.SeqSet{})
		if err != nil {
			t.Fatal(err)
		}
		got := map[uint32]bool{}
		for _, m := range msgs {
			got[m.UID] = true
		}
		return got
	}
	read()
	inWindow := false
	truncateSeam = func() { truncateSeam = nil; inWindow = true; read() }
	defer func() { truncateSeam = nil }()
	if err := a.ui.OptimizeIndex(f.ID); err != nil {
		t.Fatal(err)
	}
	if !inWindow {
		t.Fatal("the fold never reached the log replacement; the window was not read")
	}
	// The new log grows past the old one's size before the other process
	// looks again: a view extended from the old offset would skip its start.
	var added []uint32
	for i := 0; i < 40; i++ {
		added = append(added, add())
	}
	got := read()
	for i, uid := range added {
		if !got[uid] {
			t.Fatalf("the other process does not see uid %d, appended %d after the fold", uid, i+1)
		}
	}
}

// A fold lands between a read's look at the log and its extension of the view:
// the log it then opens is of another lineage, and that read answers from the
// base and the whole new log, not from the old offset into it (#2663).
func TestAFoldBeforeAnExtensionIsNotReadFromTheOldOffset(t *testing.T) {
	root := t.TempDir()
	home := testHome(root, testUser)
	open := func() *userHandle {
		return New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle)
	}
	a, b := open(), open()
	defer a.Close() //nolint:errcheck
	defer b.Close() //nolint:errcheck
	f, err := a.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	add := func() uint32 {
		uid, err := a.AllocateUID(f.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Size: 10}); err != nil {
			t.Fatal(err)
		}
		return uid
	}
	for i := 0; i < 20; i++ {
		add()
	}
	if _, err := b.OpenFolder("INBOX", 0); err != nil {
		t.Fatal(err)
	}
	read := func() map[uint32]bool {
		msgs, err := b.GetMessagesUnlocked(f.ID, mailbox.SeqSet{})
		if err != nil {
			t.Fatal(err)
		}
		got := map[uint32]bool{}
		for _, m := range msgs {
			got[m.UID] = true
		}
		return got
	}
	read()
	add() // the log grows: b's next read extends its view
	var added []uint32
	extendSeam = func() {
		extendSeam = nil
		if err := a.ui.OptimizeIndex(f.ID); err != nil {
			t.Error(err)
		}
		for i := 0; i < 40; i++ {
			added = append(added, add())
		}
	}
	defer func() { extendSeam = nil }()
	got := read()
	if extendSeam != nil {
		t.Fatal("the read did not extend its view; the window was not reached")
	}
	for i, uid := range added {
		if !got[uid] {
			t.Fatalf("the read missed uid %d, appended %d after the fold", uid, i+1)
		}
	}
}
