package lmtp

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/quota"
)

// lmtpWire is a raw LMTP client: a reply is every line up to the last one.
type lmtpWire struct {
	t  *testing.T
	c  net.Conn
	rd *bufio.Reader
}

func dialWire(t *testing.T, addr string) *lmtpWire {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck
	t.Cleanup(func() { c.Close() })
	w := &lmtpWire{t: t, c: c, rd: bufio.NewReader(c)}
	w.reply()
	return w
}

func (w *lmtpWire) reply() string {
	w.t.Helper()
	var all []string
	for {
		line, err := w.rd.ReadString('\n')
		if err != nil {
			w.t.Fatalf("read: %v (so far %q)", err, all)
		}
		line = strings.TrimRight(line, "\r\n")
		all = append(all, line)
		if len(line) < 4 || line[3] == ' ' {
			return strings.Join(all, "\n")
		}
	}
}

func (w *lmtpWire) cmd(format string, a ...any) string {
	w.t.Helper()
	fmt.Fprintf(w.c, format+"\r\n", a...)
	return w.reply()
}

// body sends DATA with a body of n bytes and returns the one recipient status.
func (w *lmtpWire) body(n int) string {
	w.t.Helper()
	if r := w.cmd("DATA"); !strings.HasPrefix(r, "354") {
		w.t.Fatalf("DATA: %s", r)
	}
	msg := "Subject: size\r\n\r\n" + strings.Repeat("x", n)
	fmt.Fprintf(w.c, "%s\r\n.\r\n", msg)
	return w.reply()
}

func sizeServer(t *testing.T, cfg config.LMTPProtocolConfig, mailSize int64, rules []string) string {
	t.Helper()
	dir := t.TempDir()
	resolver := &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n", DefaultQuotaRules: rules}
	mb := maildir.New()
	box := mb.OpenUser(resolver.UserInfo("alice@example.com", ""))
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	box.Close() //nolint:errcheck
	cfg.ReadTimeout, cfg.WriteTimeout = 5, 5
	srv := New(Options{Hostname: "lmtp.test", Config: cfg, Mailbox: mb, Index: fileindex.New(),
		Resolver: resolver, QuotaEngine: true, QuotaMailSize: mailSize})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String()
}

// quota_mail_size is offered as SIZE and enforced while the body is read, so
// an oversized message is refused without being held, and the session goes on.
func TestTheMailSizeLimitIsOfferedAndEnforcedWhileReading(t *testing.T) {
	const limit = 200
	addr := sizeServer(t, config.LMTPProtocolConfig{}, limit, nil)

	t.Run("LHLO offers SIZE", func(t *testing.T) {
		w := dialWire(t, addr)
		if r := w.cmd("LHLO mta.test"); !strings.Contains(r, fmt.Sprintf("SIZE %d", limit)) {
			t.Errorf("LHLO does not offer SIZE %d:\n%s", limit, r)
		}
	})
	t.Run("MAIL FROM SIZE over the limit is refused", func(t *testing.T) {
		w := dialWire(t, addr)
		w.cmd("LHLO mta.test")
		if r := w.cmd("MAIL FROM:<a@external.test> SIZE=%d", limit+1); !strings.HasPrefix(r, "552") {
			t.Errorf("MAIL FROM with SIZE=%d answered %q, want 552", limit+1, r)
		}
	})
	t.Run("a body over the limit is refused and the session goes on", func(t *testing.T) {
		w := dialWire(t, addr)
		w.cmd("LHLO mta.test")
		w.cmd("MAIL FROM:<a@external.test>")
		w.cmd("RCPT TO:<alice@example.com>")
		if r := w.body(limit * 2); !strings.HasPrefix(r, "552 5.3.4") {
			t.Errorf("an oversized body answered %q, want 552 5.3.4 while reading", r)
		}
		if r := w.cmd("MAIL FROM:<a@external.test>"); !strings.HasPrefix(r, "250") {
			t.Fatalf("the next transaction was refused: %q", r)
		}
		w.cmd("RCPT TO:<alice@example.com>")
		if r := w.body(10); !strings.HasPrefix(r, "250") {
			t.Errorf("a small message after the refusal answered %q", r)
		}
	})
}

// A full mailbox bounces unless quota_full_tempfail asks the MTA to retry.
func TestAFullMailboxIsPermanentUnlessTempfailIsOn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tempfail bool
		want     string
	}{
		{"default", false, "552 5.2.2"},
		{"quota_full_tempfail", true, "452 4.2.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := sizeServer(t, config.LMTPProtocolConfig{QuotaFullTempfail: tc.tempfail}, 0, []string{"*:storage=10"})
			w := dialWire(t, addr)
			w.cmd("LHLO mta.test")
			w.cmd("MAIL FROM:<a@external.test>")
			w.cmd("RCPT TO:<alice@example.com>")
			if r := w.body(100); !strings.HasPrefix(r, tc.want) {
				t.Errorf("over quota answered %q, want %s", r, tc.want)
			}
		})
	}
}

// A rate-limited recipient is a transient refusal of that recipient, not the
// server closing the channel: the session keeps answering.
func TestARateLimitedRecipientGets451AndTheSessionGoesOn(t *testing.T) {
	dir := t.TempDir()
	resolver := &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"}
	mb := maildir.New()
	box := mb.OpenUser(resolver.UserInfo("alice@example.com", ""))
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	box.Close() //nolint:errcheck
	cfg := config.LMTPProtocolConfig{ReadTimeout: 5, WriteTimeout: 5}
	cfg.RateLimit.Enabled, cfg.RateLimit.PerRecipientBurst, cfg.RateLimit.PerRecipientWindowSeconds = true, 1, 60
	srv := New(Options{Hostname: "lmtp.test", Config: cfg, Mailbox: mb, Index: fileindex.New(),
		Resolver: resolver, Locker: newFakeLocker()})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = srv.Serve(ln) }()

	w := dialWire(t, ln.Addr().String())
	w.cmd("LHLO mta.test")
	w.cmd("MAIL FROM:<a@external.test>")
	if r := w.cmd("RCPT TO:<alice@example.com>"); !strings.HasPrefix(r, "250") {
		t.Fatalf("the first RCPT was refused: %q", r)
	}
	if r := w.cmd("RCPT TO:<alice@example.com>"); !strings.HasPrefix(r, "451 4.7.0") {
		t.Errorf("a rate-limited RCPT answered %q, want 451 4.7.0", r)
	}
	if r := w.cmd("RCPT TO:<bob@example.com>"); len(r) < 3 {
		t.Errorf("the next RCPT got no answer: %q", r)
	}
}

// Grace lets the delivery that crosses the limit through, and none after it:
// a mailbox already over its limit is full however much grace is left.
func TestGraceDoesNotDeliverIntoAMailboxAlreadyOver(t *testing.T) {
	dir := t.TempDir()
	resolver := &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n", DefaultQuotaRules: []string{"*:bytes=1000"}}
	mb := maildir.New()
	box := mb.OpenUser(resolver.UserInfo("alice@example.com", ""))
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	box.Close() //nolint:errcheck
	srv := New(Options{Hostname: "lmtp.test", Config: config.LMTPProtocolConfig{ReadTimeout: 5, WriteTimeout: 5},
		Mailbox: mb, Index: fileindex.New(), Resolver: resolver, QuotaEngine: true,
		QuotaPolicy: quota.Policy{StorageGrace: 10 << 20}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = srv.Serve(ln) }()

	deliver := func() string {
		w := dialWire(t, ln.Addr().String())
		w.cmd("LHLO mta.test")
		w.cmd("MAIL FROM:<a@external.test>")
		w.cmd("RCPT TO:<alice@example.com>")
		return w.body(1500)
	}
	if r := deliver(); !strings.HasPrefix(r, "250") {
		t.Fatalf("the delivery that crosses the limit within grace was refused: %q", r)
	}
	if r := deliver(); !strings.HasPrefix(r, "552 5.2.2") {
		t.Errorf("a delivery into a mailbox already over its limit answered %q, want 552 5.2.2", r)
	}
}
