package imap_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	imap "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A record recovered by the uid-name pass reports a size, and it is the size of
// the body the same FETCH returns: a record that lost its sidecar reported zero
// and handed out nothing (#1726).
func TestARecoveredRecordReportsTheSizeOfItsBody(t *testing.T) {
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
	const body = "From: a@b\r\nSubject: recovered\r\n\r\nbody\r\n"
	base := fmt.Sprintf("1700000001.M1P1_1.host,S=%d,W=%d", len(body), len(body))
	name := base + ":2,"
	if err := os.WriteFile(filepath.Join(home, "Maildir", "cur", name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(base))
	var guid [16]byte
	copy(guid[:], sum[:16])
	// The damaged shape: no size in the record, no entry in the list.
	if err := idx.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, GUID: guid}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	if err := box.Close(); err != nil {
		t.Fatal(err)
	}

	c := startTestServerIn(t, root)
	if err := c.Login("user@test.com", "testpass").Wait(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Logout().Wait() }()
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	msgs, err := c.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{
		RFC822Size:  true,
		BodySection: []*imap.FetchItemBodySection{{}},
	}).Collect()
	if err != nil {
		t.Fatalf("FETCH: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("FETCH returned %d messages, want 1", len(msgs))
	}
	var got []byte
	for _, v := range msgs[0].BodySection {
		got = v.Bytes
	}
	if len(got) == 0 {
		t.Fatal("the message body came back empty")
	}
	if msgs[0].RFC822Size != int64(len(got)) {
		t.Errorf("RFC822.SIZE is %d, the body is %d bytes", msgs[0].RFC822Size, len(got))
	}
}
