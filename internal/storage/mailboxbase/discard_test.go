package mailboxbase_test

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// discardRecorder is a driver that takes back saves itself, and keeps what
// it was asked to take back.
type discardRecorder struct {
	mailbox.UserMailbox
	folder, saved string
	m             *mailbox.MessageMeta
	removed       bool
}

func (d *discardRecorder) DiscardSaved(folder, saved string, m *mailbox.MessageMeta) error {
	d.folder, d.saved, d.m = folder, saved, m
	return nil
}

func (d *discardRecorder) Remove(string, string) error { d.removed = true; return nil }

// Discard hands the driver both the name Save gave and the record the cycle
// tried: the second is what finds a body the cycle already renamed.
func TestDiscardReachesTheDriverWithNameAndRecord(t *testing.T) {
	d := &discardRecorder{}
	m := &mailbox.MessageMeta{UID: 4, GUID: [16]byte{1}}
	if err := mailboxbase.Open(d, nil).Discard("INBOX", "saved-name", m); err != nil {
		t.Fatal(err)
	}
	if d.removed || d.folder != "INBOX" || d.saved != "saved-name" || d.m != m {
		t.Fatalf("driver saw removed=%v folder=%q saved=%q record=%v", d.removed, d.folder, d.saved, d.m)
	}
}
