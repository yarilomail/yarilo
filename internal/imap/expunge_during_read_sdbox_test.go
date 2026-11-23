package imap_test

import (
	"path/filepath"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// On sdbox, a message another session expunges while this one reads it is
// gone, not unreadable, and the folder is not marked corrupt: sdbox calls a
// vanished u.<uid> corruption, and the box tells the expunge apart.
func TestASdboxReadThatLostToAnExpungeIsGoneNotCorrupt(t *testing.T) {
	dir := t.TempDir()
	a, b := startTestServerInWith(t, dir, dboxv2.New()), startTestServerInWith(t, dir, dboxv2.New())
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
	fired := 0
	defer dboxv2.SetTestBeforeOpen(func() {
		if fired > 0 {
			return
		}
		fired++
		done := make(chan error, 1)
		go func() {
			if err := b.Store(imaplib.SeqSetNum(1), &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: []imaplib.Flag{imaplib.FlagDeleted}}, nil).Close(); err != nil {
				done <- err
				return
			}
			done <- b.Expunge().Close()
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("the other session's expunge did not finish")
		}
	})()
	gone, unr := unreadableByReason(t, "fetch", "gone"), unreadableByReason(t, "fetch", "unreadable")
	if _, err := a.Fetch(imaplib.SeqSetNum(1), &imaplib.FetchOptions{
		BodySection: []*imaplib.FetchItemBodySection{{Specifier: imaplib.PartSpecifierText, Peek: true}},
	}).Collect(); err != nil {
		t.Fatal(err)
	}
	if fired != 1 {
		t.Fatalf("the expunge ran %d times, want 1", fired)
	}
	if got := unreadableByReason(t, "fetch", "unreadable") - unr; got != 0 {
		t.Errorf("counted %v unreadable for a message another session expunged", got)
	}
	if got := unreadableByReason(t, "fetch", "gone") - gone; got != 1 {
		t.Errorf("counted %v gone, want 1", got)
	}
	idx := file.New().OpenUser(&mailbox.UserInfo{Username: "user@test.com", Home: filepath.Join(dir, "test.com", "user")})
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	if f.Fsckd {
		t.Error("an expunge marked the folder corrupt")
	}
}
