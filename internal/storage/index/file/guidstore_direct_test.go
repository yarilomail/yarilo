package file

import (
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Delivery and APPEND settle the uid outside indexTx, and a search resolves
// every hit through this store: a copy missing here is a message no search can
// name, however well it is indexed (#1986).
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
