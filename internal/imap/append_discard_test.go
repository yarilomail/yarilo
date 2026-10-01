package imap_test

import (
	"bytes"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/yarilomail/yarilo/internal/auth/authtest"
	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// refusingRecords hands out user indexes that refuse every new record.
type refusingRecords struct{ mailbox.IndexBackend }

func (r refusingRecords) OpenUser(info *mailbox.UserInfo) mailbox.UserIndex {
	return refusedAppend{r.IndexBackend.OpenUser(info)}
}

type refusedAppend struct{ mailbox.UserIndex }

func (refusedAppend) AllocateAndAppend(uint64, *mailbox.MessageMeta) error {
	return errors.New("record refused")
}

// An APPEND whose record is refused leaves no body on disk; maildir keeps an
// unrecorded one in tmp/, which a plain removal does not look into.
func TestARefusedAppendLeavesNoBody(t *testing.T) {
	dir := t.TempDir()
	srv := imapserver.New(imapserver.Options{
		Mailbox:   maildir.New(),
		Index:     refusingRecords{file.New()},
		Resolver:  &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"},
		AuthRelay: authtest.RelayTo(t, &quotaAuthStub{user: "user@test.com", pass: "testpass", rule: "*:bytes=100000"}),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	c := imapclient.New(conn, nil)
	if err := c.Login("user@test.com", "testpass").Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	const marker = "refused-append-marker"
	msg := "From: s@x\r\nSubject: refused\r\n\r\n" + marker + "\r\n"
	ac := c.Append("INBOX", int64(len(msg)), nil)
	if _, err := ac.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	if err := ac.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Wait(); err == nil {
		t.Fatal("APPEND answered OK with its record refused")
	}

	var left []string
	if err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if b, rerr := os.ReadFile(p); rerr == nil && bytes.Contains(b, []byte(marker)) {
			left = append(left, p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("a refused APPEND left its body on disk: %v", left)
	}
}
