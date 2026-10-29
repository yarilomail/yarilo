package file

import (
	"os"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// recentFolder is INBOX with uids 1..n appended through one handle.
func recentFolder(t *testing.T, root string, n uint32) (*userIndex, *mailbox.Folder) {
	t.Helper()
	u := openIdx(root, testUser)
	t.Cleanup(func() { _ = u.Close() })
	f, err := u.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	for uid := uint32(1); uid <= n; uid++ {
		if _, err := u.AllocateUID(f.ID); err != nil {
			t.Fatal(err)
		}
		if err := u.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Size: 10}); err != nil {
			t.Fatal(err)
		}
	}
	return u, f
}

func snapshotOf(t *testing.T, u *userIndex) *mailbox.Folder {
	t.Helper()
	f, err := u.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// An index that never recorded the mark makes every message recent once; the
// claim moves it past the last UID, and another handle reads the move from the
// journal.
func TestTheRecentMarkMovesOnceAndIsSeenElsewhere(t *testing.T) {
	root := t.TempDir()
	u, f := recentFolder(t, root, 3)
	if got := snapshotOf(t, u); got.FirstRecentUID != 1 || got.Recent != 3 {
		t.Fatalf("fresh index: mark %d, recent %d; want 1 and 3", got.FirstRecentUID, got.Recent)
	}
	from, to, err := u.ClaimRecent(f.ID)
	if err != nil || from != 1 || to != 4 {
		t.Fatalf("first claim = [%d, %d) %v, want [1, 4)", from, to, err)
	}
	other := openIdx(root, testUser)
	defer other.Close() //nolint:errcheck
	if got := snapshotOf(t, other); got.FirstRecentUID != 4 || got.Recent != 0 {
		t.Errorf("another handle after the claim: mark %d, recent %d; want 4 and 0", got.FirstRecentUID, got.Recent)
	}
	from, to, err = other.ClaimRecent(f.ID)
	if err != nil || from != to {
		t.Errorf("second claim = [%d, %d) %v, want an empty range", from, to, err)
	}
}

// A claim with nothing new leaves the journal as it is.
func TestAnEmptyClaimWritesNothing(t *testing.T) {
	u, f := recentFolder(t, t.TempDir(), 2)
	if _, _, err := u.ClaimRecent(f.ID); err != nil {
		t.Fatal(err)
	}
	fs, err := u.state(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	logPath := fs.indexPath + ".log"
	before, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := u.ClaimRecent(f.ID); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Errorf("an empty claim grew the journal %d -> %d bytes", before.Size(), after.Size())
	}
}

// A rebuild of the records keeps the mark: what was claimed stays claimed.
func TestAResetKeepsTheRecentMark(t *testing.T) {
	u, f := recentFolder(t, t.TempDir(), 2)
	if _, _, err := u.ClaimRecent(f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := u.ResetFolder(f.ID, []*mailbox.MessageMeta{{UID: 1, Size: 10}, {UID: 2, Size: 10}}); err != nil {
		t.Fatal(err)
	}
	if got := snapshotOf(t, u); got.FirstRecentUID != 3 || got.Recent != 0 {
		t.Errorf("after a reset: mark %d, recent %d; want 3 and 0", got.FirstRecentUID, got.Recent)
	}
}
