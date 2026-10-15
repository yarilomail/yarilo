package file

import (
	"errors"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The store is whole only after a rebuild from the folders, stays whole through
// the commands it takes, and stops claiming it once one is missed.
func TestTheGUIDStoreCompletenessMark(t *testing.T) {
	rec := mailbox.GUIDRecord{GUID: [16]byte{1}, FolderGUID: [16]byte{7}, UID: 1}
	cases := []struct {
		name string
		do   func(t *testing.T, ui *userIndex)
		want bool
	}{
		{"no store", func(*testing.T, *userIndex) {}, false},
		{"created by a delivery", func(t *testing.T, ui *userIndex) {
			must(t, ui.AppendGUIDRecord(rec))
		}, false},
		{"rebuilt", func(t *testing.T, ui *userIndex) {
			must(t, ui.ReplaceGUIDStore([]mailbox.GUIDRecord{rec}))
		}, true},
		{"rebuilt, then commands taken", func(t *testing.T, ui *userIndex) {
			must(t, ui.ReplaceGUIDStore([]mailbox.GUIDRecord{rec}))
			must(t, ui.applyGUIDBatch(guidBatch{add: []mailbox.GUIDRecord{{GUID: [16]byte{2}, FolderGUID: [16]byte{7}, UID: 2}}}))
			must(t, ui.applyGUIDBatch(guidBatch{gone: []guidCopy{{folderGUID: [16]byte{7}, uid: 1}}}))
		}, true},
		{"rebuilt, then a command missed", func(t *testing.T, ui *userIndex) {
			must(t, ui.ReplaceGUIDStore([]mailbox.GUIDRecord{rec}))
			ui.guidTrackFailed("INBOX", errors.New("disk full"))
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ui, home := guidUser(t)
			// Another process holding the store open must see the mark move too.
			other := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
			if _, err := other.GUIDStoreComplete(); err != nil {
				t.Fatal(err)
			}
			tc.do(t, ui)
			for who, idx := range map[string]*userIndex{"writer": ui, "other": other} {
				got, err := idx.GUIDStoreComplete()
				if err != nil {
					t.Fatal(err)
				}
				if got != tc.want {
					t.Errorf("%s reads complete %v, want %v", who, got, tc.want)
				}
			}
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// A reader that already holds the store open sees the mark go: the writer and
// the JMAP session are different processes.
func TestAnOpenReaderSeesTheMarkGo(t *testing.T) {
	ui, home := guidUser(t)
	must(t, ui.ReplaceGUIDStore([]mailbox.GUIDRecord{{GUID: [16]byte{1}, FolderGUID: [16]byte{7}, UID: 1}}))
	other := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	if got, err := other.GUIDStoreComplete(); err != nil || !got {
		t.Fatalf("the reader sees complete %v, %v after the rebuild", got, err)
	}
	ui.guidTrackFailed("INBOX", errors.New("disk full"))
	if got, err := other.GUIDStoreComplete(); err != nil || got {
		t.Errorf("the reader still sees complete %v, %v after a missed command", got, err)
	}
}
