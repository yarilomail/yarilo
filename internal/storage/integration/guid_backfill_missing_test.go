package integration_test

import (
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A legacy record whose message storage cannot name is passed over by the
// backfill: the others take their GUIDs, and it gets none invented.
func TestBackfillPassesOverARecordStorageCannotName(t *testing.T) {
	mb, idx, folder := stageLegacyFolder(t, 2)
	if err := idx.AppendMessage(folder.ID, &mailbox.MessageMeta{UID: 99, Size: 10}); err != nil {
		t.Fatal(err)
	}
	if err := mailboxbase.Open(mb, idx).BackfillGUIDs(folder, "INBOX"); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	var zero [16]byte
	for uid, g := range guidsByUID(t, idx, folder.ID) {
		if uid == 99 && g != zero {
			t.Errorf("uid 99 has no message and took GUID %x", g)
		}
		if uid != 99 && g == zero {
			t.Errorf("uid %d kept a zero GUID", uid)
		}
	}
}
