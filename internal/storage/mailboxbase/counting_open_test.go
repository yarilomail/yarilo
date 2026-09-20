package mailboxbase_test

import (
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/quota"
)

// Counting a user's usage needs the folder's id and the index behind it, and
// nothing from the store: the reference opens every folder read-only and never
// syncs (quota-count.c:41). Through a session box ours walked cur/ and new/ for
// every folder of the account, on every count (#1875).
func TestCountUsageSettlesNothing(t *testing.T) {
	box, inbox := gateSetup(t)
	deliverOutOfBand(t, inbox, "1700000000.M1P1.h:2,", time.Now().Add(-time.Hour))

	// A first open takes what the store holds, so the count below is measured
	// against a folder that is already settled.
	if _, err := box.Folder("INBOX", 0); err != nil {
		t.Fatalf("settle first: %v", err)
	}
	deliverOutOfBand(t, inbox, "1700000001.M2P2.h:2,", time.Now().Add(-time.Hour))

	scanned := syncCount(t, "scanned")
	untokened := syncCount(t, "scanned-untokened")
	u := quota.CountUsage(box, box.(interface {
		Index() mailbox.UserIndex
	}).Index().(quota.FolderVSizer), []string{"INBOX"}, quota.Limits{})

	if got := syncCount(t, "scanned") - scanned; got != 0 {
		t.Errorf("the count walked the store %v times; it needs the index, not a settled folder", got)
	}
	if got := syncCount(t, "scanned-untokened") - untokened; got != 0 {
		t.Errorf("the count walked an untokened folder %v times", got)
	}
	// The count still answers: a zero here would be a folder nobody opened.
	if u.Messages != 1 {
		t.Errorf("counted %d messages, want the one the index holds", u.Messages)
	}
}

// The mechanism both call sites use: the same box, asked for a folder's
// identity, walks the store one way and not the other. inboxGUID reads a GUID
// through it and the migrator scans the folder itself (#1875).
func TestCountingViewOpensWithoutWalking(t *testing.T) {
	box, inbox := gateSetup(t)
	deliverOutOfBand(t, inbox, "1700000002.M3P3.h:2,", time.Now().Add(-time.Hour))

	scanned := syncCount(t, "scanned")
	f, err := mailbox.Counting(box).Folder("INBOX", 0)
	if err != nil {
		t.Fatalf("counting open: %v", err)
	}
	if got := syncCount(t, "scanned") - scanned; got != 0 {
		t.Errorf("the counting view walked the store %v times", got)
	}
	if f.GUID == ([16]byte{}) {
		t.Error("the counting view answered without the folder's identity, which is what the caller came for")
	}

	// The session box on the same folder still settles: this is a view, not a
	// change to what an open means.
	scanned = syncCount(t, "scanned")
	if _, err := box.Folder("INBOX", 0); err != nil {
		t.Fatalf("session open: %v", err)
	}
	if got := syncCount(t, "scanned") - scanned; got != 1 {
		t.Errorf("the session open walked the store %v times, want 1", got)
	}
}
