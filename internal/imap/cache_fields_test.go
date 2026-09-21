package imap_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	imap "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxmetrics"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

const cachedTestBody = "From: Ann <ann@example.com>\r\n" +
	"To: Bo <bo@example.org>\r\n" +
	"Subject: a listing\r\n" +
	"Message-ID: <listing@example.com>\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"two lines\r\nof body\r\n"

// cachedFolder is one maildir message with a record that carries no sizes, so
// the numbers have to come from somewhere -- storage the first time, the cache
// after that.
func cachedFolder(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "test.com", "user")
	info := &mailbox.UserInfo{Username: "user@test.com", Home: home, Driver: "maildir"}

	box := maildir.New().OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	idx := file.New().OpenUser(info)
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("1700000001.M1P1.host,S=%d,W=%d:2,", len(cachedTestBody), len(cachedTestBody))
	if err := os.WriteFile(filepath.Join(home, "Maildir", "cur", name), []byte(cachedTestBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := idx.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := mailbox.Driver(box).(mailbox.UIDNamer).AssignUID("INBOX", name, 1); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	if err := box.Close(); err != nil {
		t.Fatal(err)
	}
	return root
}

func listingFetch(t *testing.T, root string) (size int64, envelope *imap.Envelope, bs imap.BodyStructure, opens float64) {
	t.Helper()
	c := startTestServerIn(t, root)
	if err := c.Login("user@test.com", "testpass").Wait(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Logout().Wait() }()
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	before := mailboxmetrics.MessageOpens("maildir")
	msgs, err := c.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
		RFC822Size:    true,
		Envelope:      true,
		BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
	}).Collect()
	if err != nil {
		t.Fatalf("FETCH: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("FETCH returned %d messages, want 1", len(msgs))
	}
	return msgs[0].RFC822Size, msgs[0].Envelope, msgs[0].BodyStructure,
		mailboxmetrics.MessageOpens("maildir") - before
}

// Trap (a): a listing of a folder we wrote answers its sizes, envelope and
// body structure out of the cache, opening no message at all. The first
// listing is what fills the cache, so the second is the one that has to cost
// nothing (#1714).
func TestASecondListingOpensNoMessage(t *testing.T) {
	root := cachedFolder(t)

	size, env, bs, opens := listingFetch(t, root)
	if opens == 0 {
		t.Fatal("the first listing opened nothing, so the second proves nothing")
	}
	if size != int64(len(cachedTestBody)) {
		t.Errorf("RFC822.SIZE = %d, want %d", size, len(cachedTestBody))
	}
	if env == nil || env.Subject != "a listing" {
		t.Fatalf("envelope = %+v", env)
	}
	// Bracket-less on the wire, bracketed in the cache: the two conventions
	// meet at the codec, and a listing must not show the difference.
	if env.MessageID != "listing@example.com" {
		t.Fatalf("the first listing lost the message id: %+v", env)
	}
	if bs == nil {
		t.Fatal("no body structure in the first listing")
	}

	size2, env2, bs2, opens2 := listingFetch(t, root)
	if opens2 != 0 {
		t.Errorf("the second listing opened %v messages; the cache answered none of it", opens2)
	}
	if size2 != size {
		t.Errorf("RFC822.SIZE from the cache is %d, storage said %d", size2, size)
	}
	if env2 == nil || env2.Subject != env.Subject || env2.MessageID != env.MessageID {
		t.Errorf("envelope from the cache = %+v, want %+v", env2, env)
	}
	if bs2 == nil || bs2.MediaType() != bs.MediaType() {
		t.Errorf("body structure from the cache = %+v, want %+v", bs2, bs)
	}
}
