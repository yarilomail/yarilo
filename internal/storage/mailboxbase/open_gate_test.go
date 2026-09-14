package mailboxbase_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	fileidx "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// gateSetup is one account's maildir with INBOX made, and the box a session
// opens it through.
func gateSetup(t *testing.T) (mailbox.Box, string) {
	t.Helper()
	root := t.TempDir()
	info := &mailbox.UserInfo{Username: "u@x.com", Home: filepath.Join(root, "x.com", "u")}
	box := maildir.New().OpenUser(info)
	t.Cleanup(func() { box.Close() }) //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := box.Create("INBOX"); err != nil {
		t.Fatalf("create INBOX: %v", err)
	}
	idx := fileidx.New().OpenUser(info)
	t.Cleanup(func() { idx.Close() }) //nolint:errcheck
	if _, err := idx.OpenFolder("INBOX", 1); err != nil {
		t.Fatalf("open folder: %v", err)
	}
	t.Cleanup(mailboxbase.SetTestSyncTokens(8))
	// No MailPath from userdb, so INBOX is the maildir root <home>/Maildir.
	return mailboxbase.Open(box, idx), filepath.Join(info.Home, "Maildir")
}

// settle backdates cur/ and new/: without it every token carries the
// same-second dirty nonce and the gate could never be observed holding.
func settle(t *testing.T, inbox string, at time.Time) {
	t.Helper()
	for _, sub := range []string{"cur", "new"} {
		if err := os.Chtimes(filepath.Join(inbox, sub), at, at); err != nil {
			t.Fatalf("chtimes %s: %v", sub, err)
		}
	}
}

// deliverOutOfBand drops a file into cur/ as a second MUA does, backdated so
// the scan that follows is provoked by the moved mtime, not the dirty rule.
func deliverOutOfBand(t *testing.T, inbox, name string, at time.Time) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(inbox, "cur", name), []byte("Subject: x\r\n\r\nbody\r\n"), 0o600); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	settle(t, inbox, at)
}

func syncCount(t *testing.T, result string) float64 {
	t.Helper()
	return testutil.ToFloat64(mailboxbase.MetricReconcile.WithLabelValues(result))
}

func messageCount(t *testing.T, b mailbox.Box) int {
	t.Helper()
	f, err := b.(*mailboxbase.Box).Index().OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	msgs, err := b.(*mailboxbase.Box).Index().GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	return len(msgs)
}

// Both halves pinned: an untouched folder costs no walk, an out-of-band file is
// still picked up. Either alone is satisfiable by a broken gate (#1265).
func TestTheGateScansOnlyWhenTheFolderChanged(t *testing.T) {
	settled := time.Unix(1700000000, 0)

	tests := []struct {
		name         string
		change       func(t *testing.T, inbox string)
		wantScanned  float64
		wantSkipped  float64
		wantMessages int
	}{
		{
			name:         "untouched folder is not walked again",
			change:       func(*testing.T, string) {},
			wantScanned:  0,
			wantSkipped:  1,
			wantMessages: 0,
		},
		{
			name: "file dropped into cur out of band is imported",
			change: func(t *testing.T, inbox string) {
				deliverOutOfBand(t, inbox, "1700000001.M1P1_1.host:2,S", settled.Add(time.Minute))
			},
			wantScanned:  1,
			wantSkipped:  0,
			wantMessages: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, inbox := gateSetup(t)
			settle(t, inbox, settled)

			// The first open has no cached token and always walks; it is the
			// baseline, not part of the assertion.
			if _, err := b.Folder("INBOX", 0); err != nil {
				t.Fatal(err)
			}
			scanned, skipped := syncCount(t, "scanned"), syncCount(t, "skipped")

			tc.change(t, inbox)

			if _, err := b.Folder("INBOX", 0); err != nil {
				t.Fatal(err)
			}
			if got := syncCount(t, "scanned") - scanned; got != tc.wantScanned {
				t.Errorf("scans = %v, want %v", got, tc.wantScanned)
			}
			if got := syncCount(t, "skipped") - skipped; got != tc.wantSkipped {
				t.Errorf("skips = %v, want %v", got, tc.wantSkipped)
			}
			if got := messageCount(t, b); got != tc.wantMessages {
				t.Errorf("messages = %d, want %d", got, tc.wantMessages)
			}
		})
	}
}

// The quiet case at workload length: a gate that holds once and then lets go
// passes the single-pass check and fails here.
func TestTheGateHoldsAcrossManyQuietOpens(t *testing.T) {
	const opens = 20

	b, inbox := gateSetup(t)
	settle(t, inbox, time.Unix(1700000000, 0))

	if _, err := b.Folder("INBOX", 0); err != nil {
		t.Fatal(err)
	}
	scanned, skipped := syncCount(t, "scanned"), syncCount(t, "skipped")

	for i := 0; i < opens; i++ {
		if _, err := b.Folder("INBOX", 0); err != nil {
			t.Fatal(err)
		}
	}

	if got := syncCount(t, "scanned") - scanned; got != 0 {
		t.Errorf("%v walks over %d quiet opens, want 0", got, opens)
	}
	if got := syncCount(t, "skipped") - skipped; got != opens {
		t.Errorf("skips = %v, want %d", got, opens)
	}
}
