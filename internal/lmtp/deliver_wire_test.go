package lmtp

import (
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A message delivered over the wire is readable from its record afterwards, on
// every driver: nothing carries a name between the two (#1700).
func TestADeliveryOverTheWireIsReadableFromItsRecord(t *testing.T) {
	for _, tc := range []struct {
		driver string
		new    func() mailbox.MailboxBackend
	}{
		{"maildir", func() mailbox.MailboxBackend { return maildir.New() }},
		{"sdbox", func() mailbox.MailboxBackend { return dboxv2.New() }},
		{"mdbox", func() mailbox.MailboxBackend { return mdbox.New() }},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dir := t.TempDir()
			driver := tc.driver
			srv := New(testOpts(Options{
				Hostname: "lmtp.test",
				Config: config.LMTPProtocolConfig{
					AddReceivedHeader: false, ReadTimeout: 5, WriteTimeout: 5,
				},
				Mailbox:  tc.new(),
				Index:    fileindex.New(),
				Resolver: &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"},
				UserdbLookup: func(_ context.Context, user string) (*mailbox.UserInfo, error) {
					return &mailbox.UserInfo{
						Username: user, Home: filepath.Join(dir, user), Driver: driver,
					}, nil
				},
			}))
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ln.Close() }) //nolint:errcheck
			go func() { _ = srv.Serve(ln) }()

			conn, sc := dialLMTP(t, ln.Addr().String())
			sendLHLO(t, conn, sc)
			body := "Subject: over the wire\r\n\r\n" + driver + " body\r\n"
			resp := deliver(t, conn, sc, "sender@external.com", "alice@example.com", body)
			if len(resp) == 0 || resp[0][0] != '2' {
				t.Fatalf("delivery not accepted: %v", resp)
			}
			fmt.Fprintf(conn, "QUIT\r\n") //nolint:errcheck

			info := &mailbox.UserInfo{
				Username: "alice@example.com",
				Home:     filepath.Join(dir, "alice@example.com"), Driver: driver,
			}
			box := tc.new().OpenUser(info)
			defer box.Close() //nolint:errcheck
			idx := fileindex.New().OpenUser(info)
			defer idx.Close() //nolint:errcheck
			f, err := idx.OpenFolder("INBOX", 0)
			if err != nil {
				t.Fatal(err)
			}
			msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 1 {
				t.Fatalf("the folder holds %d records after one delivery", len(msgs))
			}
			rc, err := mailboxbase.OpenMessage(box, "INBOX", msgs[0])
			if err != nil {
				t.Fatalf("uid %d cannot be read from its record: %v", msgs[0].UID, err)
			}
			defer rc.Close() //nolint:errcheck
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatal(err)
			}
			// The wire adds the terminating CRLF of the DATA dot line.
			if want := body + "\r\n"; string(got) != want {
				t.Errorf("read back %q, want %q", got, want)
			}
		})
	}
}

// <Postmaster> carries no domain (RFC 5321 §4.1.1.3): userdb is asked for the
// name as it is, and only its answer decides between delivery and 550 5.1.1.
func TestADomainlessPostmasterIsLookedUpAsItIs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		known bool
		want  string
	}{
		{"known to userdb", true, "250 "},
		{"unknown to userdb", false, "550 5.1.1 "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			var asked []string
			srv := New(testOpts(Options{
				Hostname: "lmtp.test",
				Config: config.LMTPProtocolConfig{
					AddReceivedHeader: false, ReadTimeout: 5, WriteTimeout: 5,
				},
				Mailbox:  maildir.New(),
				Index:    fileindex.New(),
				Resolver: &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"},
				UserdbLookup: func(_ context.Context, user string) (*mailbox.UserInfo, error) {
					asked = append(asked, user)
					if !tc.known {
						return nil, nil
					}
					return &mailbox.UserInfo{
						Username: user, Home: filepath.Join(dir, "admin@example.com"), Driver: "maildir",
					}, nil
				},
			}))
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ln.Close() }) //nolint:errcheck
			go func() { _ = srv.Serve(ln) }()

			conn, sc := dialLMTP(t, ln.Addr().String())
			sendLHLO(t, conn, sc)
			fmt.Fprintf(conn, "MAIL FROM:<sender@external.com>\r\n") //nolint:errcheck
			sc.Scan()
			fmt.Fprintf(conn, "RCPT TO:<Postmaster>\r\n") //nolint:errcheck
			sc.Scan()
			rcpt := sc.Text()
			if tc.known {
				if !strings.HasPrefix(rcpt, "250") {
					t.Fatalf("RCPT TO:<Postmaster> answered %q", rcpt)
				}
				fmt.Fprintf(conn, "DATA\r\n") //nolint:errcheck
				sc.Scan()
				fmt.Fprintf(conn, "Subject: abuse\r\n\r\nreport\r\n.\r\n") //nolint:errcheck
				sc.Scan()
				rcpt = sc.Text()
			}
			fmt.Fprintf(conn, "QUIT\r\n") //nolint:errcheck
			if !strings.HasPrefix(rcpt, tc.want) {
				t.Errorf("the reply for <Postmaster> is %q, want %q", rcpt, tc.want)
			}
			if len(asked) != 1 || asked[0] != "Postmaster" {
				t.Errorf("userdb was asked for %q, want [\"Postmaster\"]", asked)
			}
		})
	}
}
