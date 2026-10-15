package file

import (
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// completeUser is one INBOX with one delivered message, so the store exists
// but was made by a delivery, not a rebuild.
func completeUser(t *testing.T) (*userIndex, string, uint64) {
	t.Helper()
	ui, home := guidUser(t)
	f, err := ui.OpenFolder("INBOX", 7, "")
	must(t, err)
	deliver(t, ui, f.ID, 1)
	return ui, home, f.ID
}

func deliver(t *testing.T, ui *userIndex, folderID uint64, n byte) {
	t.Helper()
	uid, err := ui.AllocateUID(folderID)
	must(t, err)
	must(t, ui.AppendMessage(folderID, &mailbox.MessageMeta{UID: uid, GUID: [16]byte{n}, InternalDate: time.Unix(1700000000, 0)}))
}

// instance is the same user opened by another process, with its own id for
// INBOX: folder ids are assigned per process.
func instance(t *testing.T, home string) (*userIndex, uint64) {
	t.Helper()
	ui := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	f, err := ui.OpenFolder("INBOX", 7, "")
	must(t, err)
	return ui, f.ID
}

// holdStore takes the store's journal as another process would.
func holdStore(t *testing.T, home string) func() {
	t.Helper()
	other, _ := instance(t, home)
	id, err := other.openGUIDStore()
	must(t, err)
	fs, err := other.state(id)
	must(t, err)
	release, err := fs.holdJournalFor("test", time.Second)
	must(t, err)
	return release
}

// shortWait makes a writer give up on a held store quickly.
func shortWait(t *testing.T) {
	old := mutLogLockWait
	mutLogLockWait = 100 * time.Millisecond
	t.Cleanup(func() { mutLogLockWait = old })
}

// The store is whole only after a rebuild, stays whole through the commands it
// takes, and stops being whole when one of a folder's commands is missed.
func TestTheGUIDStoreCompletenessMark(t *testing.T) {
	cases := []struct {
		name string
		do   func(t *testing.T, ui *userIndex, home string, inbox uint64)
		want bool
	}{
		{"created by a delivery", func(*testing.T, *userIndex, string, uint64) {}, false},
		{"rebuilt", func(t *testing.T, ui *userIndex, _ string, inbox uint64) {
			rebuilt(t, ui, inbox)
		}, true},
		{"rebuilt, then commands taken", func(t *testing.T, ui *userIndex, _ string, inbox uint64) {
			rebuilt(t, ui, inbox)
			deliver(t, ui, inbox, 2)
			must(t, ui.ExpungeMessage(inbox, 1))
		}, true},
		{"rebuilt, then a delivery gave up on the store lock", func(t *testing.T, ui *userIndex, home string, inbox uint64) {
			rebuilt(t, ui, inbox)
			shortWait(t)
			release := holdStore(t, home)
			deliver(t, ui, inbox, 2)
			release()
		}, false},
		{"rebuilt, then an expunge gave up on the store lock", func(t *testing.T, ui *userIndex, home string, inbox uint64) {
			rebuilt(t, ui, inbox)
			shortWait(t)
			release := holdStore(t, home)
			must(t, ui.ExpungeMessage(inbox, 1))
			release()
		}, false},
		{"rebuilt, then a transaction gave up on the store lock", func(t *testing.T, ui *userIndex, home string, inbox uint64) {
			rebuilt(t, ui, inbox)
			shortWait(t)
			release := holdStore(t, home)
			tx, err := ui.Begin(inbox)
			must(t, err)
			tx.Append(&mailbox.MessageMeta{GUID: [16]byte{3}, InternalDate: time.Unix(1700000000, 0)})
			_, err = tx.Commit()
			must(t, err)
			release()
		}, false},
		{"a missed command, then rebuilt", func(t *testing.T, ui *userIndex, home string, inbox uint64) {
			shortWait(t)
			release := holdStore(t, home)
			deliver(t, ui, inbox, 2)
			release()
			rebuilt(t, ui, inbox)
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ui, home, inbox := completeUser(t)
			// Another process holding the store open must read the marks too.
			reader, readerInbox := instance(t, home)
			if _, err := reader.GUIDStoreComplete([]uint64{readerInbox}); err != nil {
				t.Fatal(err)
			}
			tc.do(t, ui, home, inbox)
			for who, r := range map[string]struct {
				idx *userIndex
				id  uint64
			}{"writer": {ui, inbox}, "reader": {reader, readerInbox}} {
				got, err := r.idx.GUIDStoreComplete([]uint64{r.id})
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

// A delivery between the rebuild's read and its write waits on the store the
// rebuild holds; giving up, it marks its folder, so the store is not whole.
func TestADeliveryDuringTheRebuildLeavesItIncomplete(t *testing.T) {
	ui, home, inbox := completeUser(t)
	shortWait(t)
	other, otherInbox := instance(t, home)
	guidRebuildBetween = func() { deliver(t, other, otherInbox, 2) }
	t.Cleanup(func() { guidRebuildBetween = func() {} })
	if _, err := ui.RebuildGUIDStore([]uint64{inbox}); err != nil {
		t.Fatal(err)
	}
	got, err := ui.GUIDStoreComplete([]uint64{inbox})
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("the store is called whole, but the copy delivered during the rebuild is in neither")
	}
}

func rebuilt(t *testing.T, ui *userIndex, folders ...uint64) {
	t.Helper()
	if _, err := ui.RebuildGUIDStore(folders); err != nil {
		t.Fatal(err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
