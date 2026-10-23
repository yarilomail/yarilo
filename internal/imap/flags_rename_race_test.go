package imap_test

import (
	"sync"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
)

// A flag write commits to the index, then renames each file. A session that
// syncs between two renames sees the second message under its old name; the
// index must keep the new flags, not take the old ones back from the name.
func TestASyncBetweenAFlagWriteAndItsRenameKeepsTheNewFlags(t *testing.T) {
	cases := []struct {
		name  string
		flag  imaplib.Flag
		write func(c *imapclient.Client) error
	}{
		{"STORE", imaplib.FlagFlagged, func(c *imapclient.Client) error {
			return c.Store(imaplib.SeqSetNum(1, 2), &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: []imaplib.Flag{imaplib.FlagFlagged}}, nil).Close()
		}},
		{"FETCH BODY[] sets \\Seen", imaplib.FlagSeen, func(c *imapclient.Client) error {
			_, err := c.Fetch(imaplib.SeqSetNum(1, 2), &imaplib.FetchOptions{BodySection: []*imaplib.FetchItemBodySection{{}}}).Collect()
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
			// The first rename changes the folder, so the other session syncs;
			// the writer stops before the second.
			reached, release := make(chan struct{}), make(chan struct{})
			var mu sync.Mutex
			calls := 0
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
			written := make(chan error, 1)
			go func() { written <- tc.write(a) }()
			select {
			case <-reached:
			case <-time.After(5 * time.Second):
				t.Fatal("the write never reached its second rename")
			}
			if _, err := b.Select("INBOX", nil).Wait(); err != nil {
				t.Fatal(err)
			}
			msgs, err := b.Fetch(imaplib.SeqSetNum(1, 2), &imaplib.FetchOptions{Flags: true}).Collect()
			close(release)
			if werr := <-written; werr != nil {
				t.Fatal(werr)
			}
			if err != nil || len(msgs) != 2 {
				t.Fatalf("fetch: %v, %d messages", err, len(msgs))
			}
			for _, m := range msgs {
				has := false
				for _, f := range m.Flags {
					if f == tc.flag {
						has = true
					}
				}
				if !has {
					t.Errorf("message %d seen as %v by a session syncing before its rename: the old name took %s back", m.SeqNum, m.Flags, tc.flag)
				}
			}
		})
	}
}
