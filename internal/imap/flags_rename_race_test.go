package imap_test

import (
	"sync"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
)

// A STORE commits the flags to the index, then renames each file. A session that
// syncs between two renames sees the second message under its old name; the
// index must keep the new flags, not take the old ones back from the name.
func TestASyncBetweenTheStoreAndItsRenameKeepsTheNewFlags(t *testing.T) {
	dir := t.TempDir()
	a := startTestServerIn(t, dir)
	b := startTestServerIn(t, dir)
	for _, c := range []*imapclient.Client{a, b} {
		if err := c.Login("user@test.com", "testpass").Wait(); err != nil {
			t.Fatal(err)
		}
	}
	appendMsg(t, a, "INBOX")
	appendMsg(t, a, "INBOX")
	for _, c := range []*imapclient.Client{a, b} {
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}
	}

	// The first rename changes the folder, so the other session syncs; the
	// writer stops before the second.
	reached, release := make(chan struct{}), make(chan struct{})
	var calls int
	var mu sync.Mutex
	defer maildir.SetTestBeforeFlagRename(func() {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n != 2 {
			return
		}
		close(reached)
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	})()
	stored := make(chan error, 1)
	go func() {
		stored <- a.Store(imaplib.SeqSetNum(1, 2), &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: []imaplib.Flag{imaplib.FlagFlagged}}, nil).Close()
	}()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the STORE never reached its rename")
	}

	if _, err := b.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	msgs, err := b.Fetch(imaplib.SeqSetNum(1, 2), &imaplib.FetchOptions{Flags: true}).Collect()
	if err != nil || len(msgs) != 2 {
		t.Fatalf("fetch: %v, %d messages", err, len(msgs))
	}
	close(release)
	if err := <-stored; err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		flagged := false
		for _, f := range m.Flags {
			if f == imaplib.FlagFlagged {
				flagged = true
			}
		}
		if !flagged {
			t.Errorf("message %d seen as %v by a session syncing before its rename: the old name took the new flags back", m.SeqNum, m.Flags)
		}
	}
}
