package imap_test

import (
	"io/fs"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/yarilomail/yarilo/internal/auth/authtest"
	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// APPEND hands its date to storage, not only to the index: a rebuild of an
// sdbox folder reads R, and R must be the APPEND date (#2175).
func TestAppendDateReachesStorage(t *testing.T) {
	dir := t.TempDir()
	srv := imapserver.New(imapserver.Options{
		Mailbox:   dboxv2.New(),
		Index:     file.New(),
		Resolver:  &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"},
		AuthRelay: authtest.RelayTo(t, &stubPassdb{user: "user@test.com", pass: "testpass"}),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)                 //nolint:errcheck
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() }) //nolint:errcheck
	c := imapclient.New(conn, nil)
	if err := c.Login("user@test.com", "testpass").Wait(); err != nil {
		t.Fatal(err)
	}

	want := time.Date(2024, 11, 23, 17, 45, 9, 0, time.UTC)
	raw := []byte("From: a@b\r\nSubject: dated\r\n\r\nHello.\r\n")
	ac := c.Append("INBOX", int64(len(raw)), &imap.AppendOptions{Time: want})
	if _, err := ac.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := ac.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ac.Wait(); err != nil {
		t.Fatal(err)
	}
	c.Logout().Wait() //nolint:errcheck

	var home string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && d.Name() == "sdbox" {
			home = filepath.Dir(p)
			return filepath.SkipAll
		}
		return nil
	})
	if home == "" {
		t.Fatal("no sdbox store under the root")
	}
	store := dboxv2.New().OpenUser(&mailbox.UserInfo{Username: "user@test.com", Home: home})
	recs, err := mailbox.Driver(store).(interface {
		Scan(string) ([]mailbox.ScanRecord, error)
	}).Scan("INBOX")
	if err != nil || len(recs) != 1 {
		t.Fatalf("scan = %v, err = %v", recs, err)
	}
	if !recs[0].InternalDate.Equal(want) {
		t.Errorf("storage holds INTERNALDATE %s, want the APPEND date %s", recs[0].InternalDate.UTC(), want)
	}
}
