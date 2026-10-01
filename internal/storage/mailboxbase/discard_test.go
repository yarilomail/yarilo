package mailboxbase_test

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

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

// restoreRefuser is a driver whose disk refuses to put a moved body back.
type restoreRefuser struct{ mailbox.UserMailbox }

func (restoreRefuser) Username() string { return "alice@x" }

func (restoreRefuser) RestoreMoved(string, string, string, string, *mailbox.MessageMeta) error {
	return errors.New("disk full")
}

// A move left unrestored is a message outside its record: it is counted, and
// the error reaches the caller.
func TestAFailedRestoreIsCounted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store mailbox.UserMailbox
	}{
		{"driver refuses", restoreRefuser{}},
		// Embedding through the interface hides RestoreMoved.
		{"driver cannot restore", struct{ mailbox.UserMailbox }{restoreRefuser{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := restoreFailures(t)
			err := mailboxbase.Open(tc.store, nil).Restore("INBOX", "orig", "Other", "moved", &mailbox.MessageMeta{UID: 3})
			if err == nil {
				t.Fatal("a failed restore answered nil")
			}
			if got := restoreFailures(t) - before; got != 1 {
				t.Fatalf("counted %v failed restores, want 1", got)
			}
		})
	}
}

func restoreFailures(t *testing.T) float64 {
	t.Helper()
	fams, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, f := range fams {
		if f.GetName() == "mailbox_move_restore_failed_total" {
			for _, m := range f.GetMetric() {
				total += m.GetCounter().GetValue()
			}
		}
	}
	return total
}
