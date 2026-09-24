package file

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func guidUser(t *testing.T) (*userIndex, string) {
	t.Helper()
	home := t.TempDir()
	ui := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	return ui, home
}

// One record per copy, read back as it was written: the store is what a JMAP
// id resolves through (#1711, INTERNALS.md §35).
func TestTheGUIDStoreKeepsOneRecordPerCopy(t *testing.T) {
	ui, _ := guidUser(t)
	want := []mailbox.GUIDRecord{
		{GUID: [16]byte{1, 2, 3}, FolderID: 7, UID: 11, Flags: 1, InternalDate: 1700000000, CID: 0},
		{GUID: [16]byte{1, 2, 3}, FolderID: 9, UID: 4, Flags: 0, InternalDate: 1700000001},
		{GUID: [16]byte{9}, FolderID: 7, UID: 12, Flags: 2, InternalDate: 1700000002},
	}
	for _, r := range want {
		if err := ui.AppendGUIDRecord(r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ui.GUIDRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("the store holds %d copies, %d were written", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("copy %d reads %+v, was written %+v", i, got[i], want[i])
		}
	}
}

// The store is an index of the user, not a folder: it lives under its own name
// in the index root, so no folder enumerator can reach it.
func TestTheGUIDStoreIsNotAFolder(t *testing.T) {
	ui, home := guidUser(t)
	if err := ui.AppendGUIDRecord(mailbox.GUIDRecord{GUID: [16]byte{1}, FolderID: 1, UID: 1}); err != nil {
		t.Fatal(err)
	}
	path := ui.GUIDStorePath()
	if filepath.Dir(path) != home || filepath.Base(path) != GUIDIndexFileName {
		t.Errorf("the store is at %q, want %q in the index root", path, GUIDIndexFileName)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the store was not written: %v", err)
	}
	// A folder's index is a directory with yarilo.index in it. The store must
	// not have made one, or LIST and every other walk would report it.
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(home, e.Name(), IndexFileName)); err == nil {
			t.Errorf("the store left a folder-shaped index at %q, which every folder walk would report", e.Name())
		}
	}
}

// Reopening reads what the last process wrote: the store is a file, not a
// session's memory.
func TestTheGUIDStoreSurvivesReopening(t *testing.T) {
	ui, home := guidUser(t)
	r := mailbox.GUIDRecord{GUID: [16]byte{4, 5}, FolderID: 3, UID: 8, Flags: 5, InternalDate: 99, CID: 0}
	if err := ui.AppendGUIDRecord(r); err != nil {
		t.Fatal(err)
	}
	again := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	got, err := again.GUIDRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != r {
		t.Errorf("after reopening the store holds %+v, one copy %+v was written", got, r)
	}
}
