package file

import (
	"os"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/v2/pkg/locks/lockstest"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A foreign journal met on reload is dropped: the base's messages stay, the
// expunge floor rises, and the next append reads in every process.
func TestAForeignJournalMetOnReloadIsDropped(t *testing.T) {
	dial := lockstest.Server(t)
	home := t.TempDir()
	a, b := process(t, home, dial()), process(t, home, dial())
	f := inbox(t, a)
	for uid := uint32(1); uid <= 2; uid++ {
		must(t, a.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Size: 10}))
	}
	must(t, a.OptimizeIndex(f.ID))
	refresh := a.(mailbox.FolderRefresher).RefreshFolder
	must(t, refresh(f.ID))
	floor, err := a.ExpungeFloor(f.ID)
	if err != nil {
		t.Fatal(err)
	}

	logPath := a.(*userHandle).ui.open[f.ID].indexPath + ".log"
	hdr := mailindex.NewLogHeader(0xDEADBEEF, 1, 0)
	foreign := append(hdr.EncodeBytes(), make([]byte, 256)...) // longer than the journal it replaces
	if err := os.WriteFile(logPath, foreign, 0o600); err != nil {
		t.Fatal(err)
	}

	must(t, refresh(f.ID))
	if n := len(allOf(t, a, f.ID)); n != 2 {
		t.Errorf("%d messages after dropping the foreign journal, want 2", n)
	}
	if raised, err := a.ExpungeFloor(f.ID); err != nil || raised < floor {
		t.Errorf("floor %d after the drop (was %d), err %v", raised, floor, err)
	}
	must(t, a.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 3, Size: 10}))
	fb := inbox(t, b)
	if n := len(allOf(t, b, fb.ID)); n != 3 {
		t.Errorf("the other process reads %d after the next append, want 3", n)
	}
}
