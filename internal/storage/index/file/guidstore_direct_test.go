package file

import (
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Delivery and APPEND settle the uid outside indexTx: a copy missing here is a
// message no search can name, however well it is indexed (#1986).
func TestEveryAppendPathRecordsTheCopy(t *testing.T) {
	tests := []struct {
		name  string
		guid  [16]byte
		write func(*userIndex, uint64, *mailbox.MessageMeta) error
	}{
		{"delivery allocates the uid", [16]byte{1, 1}, func(ui *userIndex, id uint64, m *mailbox.MessageMeta) error {
			return ui.AllocateAndAppend(id, m)
		}},
		{"the uid was allocated before", [16]byte{2, 2}, func(ui *userIndex, id uint64, m *mailbox.MessageMeta) error {
			uid, err := ui.AllocateUID(id)
			if err != nil {
				return err
			}
			m.UID = uid
			return ui.AppendMessage(id, m)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ui, _, folderID := guidTxFolder(t)
			m := &mailbox.MessageMeta{GUID: tc.guid, InternalDate: time.Unix(1700000000, 0)}
			if err := tc.write(ui, folderID, m); err != nil {
				t.Fatal(err)
			}
			copies, err := ui.GUIDCopies([][16]byte{tc.guid})
			if err != nil {
				t.Fatal(err)
			}
			if len(copies) != 1 {
				t.Fatalf("the store holds %d copies of the message, want 1", len(copies))
			}
			if copies[0].UID != m.UID {
				t.Errorf("the copy names uid %d, want %d", copies[0].UID, m.UID)
			}
			if copies[0].FolderGUID == ([16]byte{}) {
				t.Error("the copy names no folder, so a folder-scoped search cannot match it")
			}
		})
	}
}

// The retraction has the same two paths: a copy expunged outside a transaction
// must leave the store, or a search answers a uid that is gone.
func TestADirectExpungeRetractsTheCopy(t *testing.T) {
	ui, _, folderID := guidTxFolder(t)
	guid := [16]byte{3, 3}
	m := &mailbox.MessageMeta{GUID: guid, InternalDate: time.Unix(1700000000, 0)}
	if err := ui.AllocateAndAppend(folderID, m); err != nil {
		t.Fatal(err)
	}
	// The copy must be there first, or "nothing is left" is true of a store
	// that never took it and the row proves nothing.
	before, err := ui.GUIDCopies([][16]byte{guid})
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("the append left %d copies, so the retraction cannot be observed", len(before))
	}
	if err := ui.ExpungeMessage(folderID, m.UID); err != nil {
		t.Fatal(err)
	}
	copies, err := ui.GUIDCopies([][16]byte{guid})
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 0 {
		t.Errorf("the store still holds %d copies of an expunged message", len(copies))
	}
}

// An identity settled after the record was written -- a restamp, an adopted
// foreign index, a rebuild -- must reach the store too.
func TestStampingRecordsTheCopy(t *testing.T) {
	ui, _, folderID := guidTxFolder(t)
	m := &mailbox.MessageMeta{UID: 1, InternalDate: time.Unix(1700000000, 0)}
	if err := ui.AppendMessage(folderID, m); err != nil {
		t.Fatal(err)
	}
	guid := [16]byte{4, 4}
	if err := ui.SetGUIDs(folderID, map[uint32][16]byte{1: guid}); err != nil {
		t.Fatal(err)
	}
	copies, err := ui.GUIDCopies([][16]byte{guid})
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 1 || copies[0].UID != 1 {
		t.Fatalf("the store holds %v for a stamped message, want one copy at uid 1", copies)
	}
}

// A rebuild replaces the folder's records and their identities in one command,
// so the store must end up holding what the folder now holds -- no more.
func TestResetFolderReplacesTheFoldersCopies(t *testing.T) {
	ui, _, folderID := guidTxFolder(t)
	old := [16]byte{5, 5}
	if err := ui.AllocateAndAppend(folderID, &mailbox.MessageMeta{GUID: old, InternalDate: time.Unix(1700000000, 0)}); err != nil {
		t.Fatal(err)
	}
	fresh := [16]byte{6, 6}
	if _, err := ui.ResetFolder(folderID, []*mailbox.MessageMeta{
		{UID: 1, GUID: fresh, InternalDate: time.Unix(1700000000, 0)},
	}); err != nil {
		t.Fatal(err)
	}
	gone, err := ui.GUIDCopies([][16]byte{old})
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 0 {
		t.Errorf("the store still holds %d copies under the identity the rebuild replaced", len(gone))
	}
	kept, err := ui.GUIDCopies([][16]byte{fresh})
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 {
		t.Errorf("the store holds %d copies of the rebuilt record, want 1", len(kept))
	}
}

// A folder that is gone holds no copies, or an account-wide search answers a
// uid in a mailbox that no longer exists. Not INBOX: it holds the store here.
func TestDeleteFolderRetractsItsCopies(t *testing.T) {
	ui, _, inboxID := guidTxFolder(t)
	kept := [16]byte{9, 9}
	if err := ui.AllocateAndAppend(inboxID, &mailbox.MessageMeta{GUID: kept, InternalDate: time.Unix(1700000000, 0)}); err != nil {
		t.Fatal(err)
	}
	other, err := ui.OpenFolder("Archive", 7, "")
	if err != nil {
		t.Fatal(err)
	}
	guid := [16]byte{8, 8}
	if err := ui.AllocateAndAppend(other.ID, &mailbox.MessageMeta{GUID: guid, InternalDate: time.Unix(1700000000, 0)}); err != nil {
		t.Fatal(err)
	}
	before, err := ui.GUIDCopies([][16]byte{guid})
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 {
		t.Fatalf("the append left %d copies, so the retraction cannot be observed", len(before))
	}
	if err := ui.DeleteFolder("Archive"); err != nil {
		t.Fatal(err)
	}
	after, err := ui.GUIDCopies([][16]byte{guid})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Errorf("the store still holds %d copies of a folder that is gone", len(after))
	}
	// Only that folder's copies: the store is the user's, not the folder's.
	survivor, err := ui.GUIDCopies([][16]byte{kept})
	if err != nil {
		t.Fatal(err)
	}
	if len(survivor) != 1 {
		t.Errorf("deleting one folder took %d copies of another folder with it", 1-len(survivor))
	}
}
