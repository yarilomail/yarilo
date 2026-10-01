package imap_test

import (
	"bytes"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/yarilomail/yarilo/internal/auth/authtest"
	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// switchedRecords refuses new records while refuse is set; afterName lets the
// driver name the body under the destination's uid first.
type switchedRecords struct {
	mailbox.IndexBackend
	refuse, afterName *atomic.Bool
}

func (r switchedRecords) OpenUser(info *mailbox.UserInfo) mailbox.UserIndex {
	return switchedAppend{r.IndexBackend.OpenUser(info), r.refuse, r.afterName}
}

type switchedAppend struct {
	mailbox.UserIndex
	refuse, afterName *atomic.Bool
}

var errSwitchedOff = errors.New("record refused")

func (a switchedAppend) AllocateAndAppend(id uint64, m *mailbox.MessageMeta) error {
	return a.AllocateAndAppendNamed(id, m, nil)
}

func (a switchedAppend) AllocateAndAppendNamed(id uint64, m *mailbox.MessageMeta, name func(uint32) (string, error)) error {
	if !a.refuse.Load() {
		return a.UserIndex.(mailbox.NamingAppender).AllocateAndAppendNamed(id, m, name)
	}
	if a.afterName.Load() && name != nil {
		m.UID = 1000
		if _, err := name(m.UID); err != nil {
			return err
		}
	}
	return errSwitchedOff
}

// A move whose destination record is refused leaves the source as it was: the
// body under the name its record holds, nothing in the destination.
func TestARefusedMoveLeavesTheSourceAsItWas(t *testing.T) {
	for _, drv := range []struct {
		name string
		new  func() mailbox.MailboxBackend
	}{
		{"maildir", func() mailbox.MailboxBackend { return maildir.New() }},
		{"sdbox", func() mailbox.MailboxBackend { return dboxv2.New() }},
		{"mdbox", func() mailbox.MailboxBackend { return mdbox.New() }},
	} {
		for _, op := range []string{"move", "move-collide", "rename-inbox"} {
			if op == "move-collide" && drv.name == "mdbox" {
				continue // a map uid is never reused, so no name collides
			}
			for _, afterName := range []bool{false, true} {
				stage := map[bool]string{false: "before-name", true: "after-name"}[afterName]
				t.Run(drv.name+"/"+op+"/"+stage, func(t *testing.T) {
					refusedMoveRow(t, drv.name, drv.new, op, afterName)
				})
			}
		}
	}
}

func refusedMoveRow(t *testing.T, driver string, newBackend func() mailbox.MailboxBackend, op string, afterName bool) {
	dir := t.TempDir()
	var refuse, named atomic.Bool
	named.Store(afterName)
	addr := switchedServer(t, dir, newBackend(), switchedRecords{file.New(), &refuse, &named})
	c := switchedLogin(t, addr)

	const marker = "moved-body-marker"
	msg := "From: s@x\r\nSubject: move\r\n\r\n" + marker + "\r\n"
	ac := c.Append("INBOX", int64(len(msg)), nil)
	if _, err := ac.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	if err := ac.Close(); err != nil {
		t.Fatal(err)
	}
	appended, err := ac.Wait()
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := c.Create("Other", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "test.com", "user")
	if op == "move-collide" && driver == "sdbox" {
		collideInDestination(t, c, driver, home)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if op == "move-collide" && driver == "maildir" {
		collideInDestination(t, c, driver, home)
	}
	before := otherCount(t, c)

	refuse.Store(true)
	switch op {
	case "move", "move-collide":
		if _, err := c.Move(imap.SeqSetNum(1), "Other").Wait(); err == nil {
			t.Fatal("MOVE answered OK with the destination record refused")
		}
	case "rename-inbox":
		if err := c.Rename("INBOX", "Old", nil).Wait(); err == nil {
			t.Fatal("RENAME INBOX answered OK with the destination record refused")
		}
	}
	refuse.Store(false)

	if driver == "mdbox" {
		// A reference not taken back shows only once a purge reclaims it.
		store := mdbox.New().OpenUser(&mailbox.UserInfo{Username: "user@test.com", Home: home, IndexDir: filepath.Join(home, "index"), Driver: "mdbox"})
		if _, err := mailbox.Driver(store).(interface {
			Purge() (mdbox.PurgeStats, error)
		}).Purge(); err != nil {
			t.Fatal(err)
		}
		_ = store.Close()
	}
	// The same session: its cached map has to follow the purge (#2100).
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	// By its uid: a body back under another name is a new message to a walk,
	// and the record the client knows is gone.
	got, err := c.Fetch(imap.UIDSetNum(appended.UID), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{}}}).Collect()
	if err != nil || len(got) != 1 || len(got[0].BodySection) == 0 || !strings.Contains(string(got[0].BodySection[0].Bytes), marker) {
		t.Fatalf("INBOX uid %d no longer opens its message: err=%v got=%d", appended.UID, err, len(got))
	}
	if held := markerFiles(t, home, marker); len(held) != 1 {
		t.Fatalf("the body is on disk %d times, want once: %v", len(held), held)
	}
	if op != "rename-inbox" {
		if after := otherCount(t, c); after != before {
			t.Fatalf("the destination holds %d messages after the rollback, %d before", after, before)
		}
	}
}

// collideInDestination puts a body in Other under the name the INBOX message
// has, so the move has to park it under another.
func collideInDestination(t *testing.T, c *imapclient.Client, driver, home string) {
	t.Helper()
	switch driver {
	case "sdbox":
		msg := "From: s@x\r\nSubject: occupant\r\n\r\nfirst in Other\r\n"
		ac := c.Append("Other", int64(len(msg)), nil)
		if _, err := ac.Write([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		if err := ac.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := ac.Wait(); err != nil {
			t.Fatal(err)
		}
	case "maildir":
		var names []os.DirEntry
		for _, sub := range []string{"cur", "new"} {
			got, _ := os.ReadDir(filepath.Join(home, "Maildir", sub))
			names = append(names, got...)
		}
		if len(names) != 1 {
			t.Fatalf("INBOX holds %d files, want 1", len(names))
		}
		occupant := filepath.Join(home, "Maildir", ".Other", "cur", names[0].Name())
		if err := os.WriteFile(occupant, []byte("Subject: occupant\r\n\r\nx\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func otherCount(t *testing.T, c *imapclient.Client) uint32 {
	t.Helper()
	st, err := c.Status("Other", &imap.StatusOptions{NumMessages: true}).Wait()
	if err != nil || st.NumMessages == nil {
		t.Fatalf("STATUS Other: %v", err)
	}
	return *st.NumMessages
}

func switchedServer(t *testing.T, dir string, mb mailbox.MailboxBackend, idx mailbox.IndexBackend) string {
	t.Helper()
	srv := imapserver.New(imapserver.Options{
		Mailbox:   mb,
		Index:     idx,
		Resolver:  &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"},
		AuthRelay: authtest.RelayTo(t, &quotaAuthStub{user: "user@test.com", pass: "testpass", rule: "*:bytes=100000"}),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

func switchedLogin(t *testing.T, addr string) *imapclient.Client {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	c := imapclient.New(conn, nil)
	if err := c.Login("user@test.com", "testpass").Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}
	return c
}

// markerFiles names every file under root whose bytes hold marker.
func markerFiles(t *testing.T, root, marker string) []string {
	t.Helper()
	var out []string
	if err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if b, rerr := os.ReadFile(p); rerr == nil && bytes.Contains(b, []byte(marker)) {
			out = append(out, p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}
