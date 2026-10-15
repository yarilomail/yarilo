package imap_test

import (
	"net"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/auth/authtest"
	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/quota"
)

// A folder takes messages up to its cap, not one short of it.
func TestAppendFillsAFolderToItsCapThenRefuses(t *testing.T) {
	srv := imapserver.New(imapserver.Options{
		Mailbox:     maildir.New(),
		Index:       file.New(),
		Resolver:    &mailbox.Resolver{Root: t.TempDir(), HomeTemplate: "%d/%n"},
		AuthRelay:   authtest.RelayTo(t, &stubPassdb{user: "user@test.com", pass: "testpass"}),
		QuotaEngine: true,
		QuotaPolicy: quota.Policy{StoragePercentage: 100, MessagePercentage: 100, MailboxMessageCount: 2},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })

	c := dialRaw(t, ln.Addr().String())
	c.login()
	for i := 1; i <= 2; i++ {
		c.appendMsg("INBOX")
	}
	if out := c.cmd(`STATUS INBOX (MESSAGES)`); !strings.Contains(out, "MESSAGES 2") {
		t.Fatalf("a folder with a cap of 2 did not take 2 messages:\n%s", out)
	}
	rc := c
	rc.seq++
	tag := "cap3"
	body := "From: a@b\r\nSubject: three\r\n\r\nx\r\n"
	rc.conn.Write([]byte(tag + " APPEND INBOX {" + itoa(len(body)) + "}\r\n")) //nolint:errcheck
	first := rc.readLine()
	if strings.HasPrefix(first, "+") {
		rc.conn.Write([]byte(body + "\r\n")) //nolint:errcheck
		first = ""
	}
	var last string
	for last = first; !strings.HasPrefix(last, tag+" "); last = rc.readLine() {
	}
	if !strings.Contains(last, "NO") || !strings.Contains(last, "OVERQUOTA") {
		t.Errorf("APPEND past the cap answered %q, want NO [OVERQUOTA]", last)
	}
}

// COPY is checked once for the whole set: two messages into a folder with room
// for one are refused, and none of them lands.
func TestCopyIsRefusedAsABatchAtTheCap(t *testing.T) {
	srv := imapserver.New(imapserver.Options{
		Mailbox:     maildir.New(),
		Index:       file.New(),
		Resolver:    &mailbox.Resolver{Root: t.TempDir(), HomeTemplate: "%d/%n"},
		AuthRelay:   authtest.RelayTo(t, &stubPassdb{user: "user@test.com", pass: "testpass"}),
		QuotaEngine: true,
		QuotaPolicy: quota.Policy{StoragePercentage: 100, MessagePercentage: 100, MailboxMessageCount: 2},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { ln.Close() })

	c := dialRaw(t, ln.Addr().String())
	c.login()
	c.cmd(`CREATE Archive`)
	c.appendMsg("INBOX")
	c.appendMsg("INBOX")
	c.appendMsg("Archive")
	c.cmd(`SELECT INBOX`)
	if out := c.cmd(`COPY 1:2 Archive`); !strings.Contains(out, "NO") || !strings.Contains(out, "OVERQUOTA") {
		t.Errorf("COPY past the cap answered %q, want NO [OVERQUOTA]", out)
	}
	if out := c.cmd(`STATUS Archive (MESSAGES)`); !strings.Contains(out, "MESSAGES 1") {
		t.Errorf("a refused COPY changed the folder:\n%s", out)
	}
}

// max_mail_size holds for every message of a COPY: one message over it refuses
// the set, and nothing lands.
func TestCopyRefusesASetHoldingAMessageOverTheMailSize(t *testing.T) {
	root := t.TempDir()
	serve := func(mailSize int64) string {
		srv := imapserver.New(imapserver.Options{
			Mailbox:       maildir.New(),
			Index:         file.New(),
			Resolver:      &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"},
			AuthRelay:     authtest.RelayTo(t, &stubPassdb{user: "user@test.com", pass: "testpass"}),
			QuotaEngine:   true,
			QuotaMailSize: mailSize,
			QuotaPolicy:   quota.Policy{StoragePercentage: 100, MessagePercentage: 100},
		})
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go srv.Serve(ln) //nolint:errcheck
		t.Cleanup(func() { ln.Close() })
		return ln.Addr().String()
	}

	seed := dialRaw(t, serve(0))
	seed.login()
	seed.cmd(`CREATE Archive`)
	seed.appendMsg("INBOX")
	big := "From: a@b\r\nSubject: big\r\n\r\n" + strings.Repeat("x", 400) + "\r\n"
	seed.seq++
	seed.conn.Write([]byte("big APPEND INBOX {" + itoa(len(big)) + "}\r\n")) //nolint:errcheck
	if line := seed.readLine(); strings.HasPrefix(line, "+") {
		seed.conn.Write([]byte(big + "\r\n")) //nolint:errcheck
	}
	for line := seed.readLine(); !strings.HasPrefix(line, "big "); line = seed.readLine() {
	}

	c := dialRaw(t, serve(200))
	c.login()
	c.cmd(`SELECT INBOX`)
	if out := c.cmd(`COPY 1:2 Archive`); !strings.Contains(out, "NO") || !strings.Contains(out, "OVERQUOTA") {
		t.Errorf("COPY of a message over max_mail_size answered %q, want NO [OVERQUOTA]", out)
	}
	if out := c.cmd(`STATUS Archive (MESSAGES)`); !strings.Contains(out, "MESSAGES 0") {
		t.Errorf("a refused COPY changed the folder:\n%s", out)
	}
}
