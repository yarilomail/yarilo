package lmtp

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A delivered message is readable from its record afterwards, on every driver:
// the record is what a reader has, and no name travels with it (#1700).
func TestADeliveredMessageIsReadableFromItsRecord(t *testing.T) {
	for _, tc := range []struct {
		driver string
		new    func() mailbox.MailboxBackend
	}{
		{"maildir", func() mailbox.MailboxBackend { return maildir.New() }},
		{"sdbox", func() mailbox.MailboxBackend { return dboxv2.New() }},
		{"mdbox", func() mailbox.MailboxBackend { return mdbox.New() }},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			root := t.TempDir()
			mb, idx := tc.new(), fileindex.New()
			info := &mailbox.UserInfo{
				Username: "alice@x", Home: filepath.Join(root, "alice"), Driver: tc.driver,
			}
			box := mb.OpenUser(info)
			defer box.Close() //nolint:errcheck
			if err := box.Init(); err != nil {
				t.Fatal(err)
			}
			ui := idx.OpenUser(info)
			defer ui.Close() //nolint:errcheck

			raw := "From: a@b\r\nSubject: delivered\r\n\r\n" + tc.driver + " body\r\n"
			if _, _, _, err := deliverOne(box, ui, "INBOX", bytes.NewReader([]byte(raw)),
				int64(len(raw)), nil, info.Username, "x@y", nil); err != nil {
				t.Fatalf("deliver: %v", err)
			}

			f, err := ui.OpenFolder("INBOX", 0)
			if err != nil {
				t.Fatal(err)
			}
			msgs, err := ui.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 1 {
				t.Fatalf("the folder holds %d records after one delivery", len(msgs))
			}
			rc, err := mailbox.OpenMessage(box, "INBOX", msgs[0])
			if err != nil {
				t.Fatalf("uid %d cannot be read from its record: %v", msgs[0].UID, err)
			}
			defer rc.Close() //nolint:errcheck
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != raw {
				t.Errorf("read back %q, want %q", got, raw)
			}
			if msgs[0].Size != uint32(len(raw)) {
				t.Errorf("size %d, want %d", msgs[0].Size, len(raw))
			}
		})
	}
}

// And the sidecar the index used to keep is not written by any of it: for
// maildir the list is the mapping, for the other two the record is (#1700).
func TestNoSidecarIsWrittenByADelivery(t *testing.T) {
	for _, tc := range []struct {
		driver string
		new    func() mailbox.MailboxBackend
	}{
		{"maildir", func() mailbox.MailboxBackend { return maildir.New() }},
		{"sdbox", func() mailbox.MailboxBackend { return dboxv2.New() }},
		{"mdbox", func() mailbox.MailboxBackend { return mdbox.New() }},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			root := t.TempDir()
			mb, idx := tc.new(), fileindex.New()
			info := &mailbox.UserInfo{
				Username: "alice@x", Home: filepath.Join(root, "alice"), Driver: tc.driver,
			}
			box := mb.OpenUser(info)
			defer box.Close() //nolint:errcheck
			if err := box.Init(); err != nil {
				t.Fatal(err)
			}
			ui := idx.OpenUser(info)
			defer ui.Close() //nolint:errcheck
			raw := "From: a@b\r\n\r\nbody\r\n"
			if _, _, _, err := deliverOne(box, ui, "INBOX", bytes.NewReader([]byte(raw)),
				int64(len(raw)), nil, info.Username, "x@y", nil); err != nil {
				t.Fatal(err)
			}

			var found []string
			err := filepath.Walk(root, func(p string, fi os.FileInfo, werr error) error {
				if werr != nil || fi.IsDir() {
					return werr
				}
				if strings.HasSuffix(p, ".names") {
					found = append(found, p)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(found) != 0 {
				t.Errorf("a delivery wrote %v", found)
			}
		})
	}
}
