package imap_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"testing"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
	imapserver "github.com/yarilomail/yarilo/internal/imap"
	"github.com/yarilomail/yarilo/internal/lmtp"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A message delivered over LMTP is fetched back over an IMAP session, byte for
// byte: the record is the only thing between the two (#1700).
func TestADeliveryOverTheWireFetchesBackOverIMAP(t *testing.T) {
	const user, pass = "user@test.com", "testpass"
	for _, be := range backends {
		t.Run(be.name, func(t *testing.T) {
			root := t.TempDir()
			driver := be.name
			res := &mailbox.Resolver{Root: root, HomeTemplate: "%d/%n"}
			mb := be.new(t)

			lsrv := lmtp.New(lmtp.Options{
				Hostname: "lmtp.test",
				Config: config.LMTPProtocolConfig{
					AddReceivedHeader: false, ReadTimeout: 5, WriteTimeout: 5,
				},
				Mailbox:  mb,
				Index:    file.New(),
				Resolver: res,
				UserdbLookup: func(_ context.Context, u string) (*mailbox.UserInfo, error) {
					ui := res.UserInfo(u, "")
					ui.Driver = driver
					return ui, nil
				},
			})
			lln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { lln.Close() }) //nolint:errcheck
			go func() { _ = lsrv.Serve(lln) }()

			body := "Subject: over the wire\r\n\r\n" + driver + " body\r\n"
			deliverLMTP(t, lln.Addr().String(), user, body)

			isrv := imapserver.New(imapserver.Options{
				Mailbox:  mailbox.Validating(be.new(t), mailbox.DefaultNameRules()),
				Index:    file.New(),
				Resolver: res,
				Auth:     &formatPassdb{user: user, pass: pass, format: driver},
			})
			iln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { iln.Close() }) //nolint:errcheck
			go isrv.Serve(iln)                //nolint:errcheck

			conn, err := net.Dial("tcp", iln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { conn.Close() }) //nolint:errcheck
			c := imapclient.New(conn, nil)
			if err := c.WaitGreeting(); err != nil {
				t.Fatal(err)
			}
			if err := c.Login(user, pass).Wait(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Logout().Wait() }()

			sel, err := c.Select("INBOX", nil).Wait()
			if err != nil {
				t.Fatalf("SELECT: %v", err)
			}
			if sel.NumMessages != 1 {
				t.Fatalf("INBOX holds %d messages after one delivery, want 1", sel.NumMessages)
			}
			seq := imap.SeqSetNum(1)
			msgs, err := c.Fetch(seq, &imap.FetchOptions{
				BodySection: []*imap.FetchItemBodySection{{}},
			}).Collect()
			if err != nil {
				t.Fatalf("FETCH BODY[]: %v", err)
			}
			if len(msgs) != 1 {
				t.Fatalf("FETCH returned %d messages, want 1", len(msgs))
			}
			var got []byte
			for _, v := range msgs[0].BodySection {
				got = v.Bytes
			}
			if want := body; string(got) != want {
				t.Errorf("FETCH BODY[] returned %q, want %q", got, want)
			}
		})
	}
}

// deliverLMTP speaks the dialogue by hand: the row is about bytes that crossed a
// socket, which a call into the session would not assert.
func deliverLMTP(t *testing.T, addr, rcpt, body string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck
	sc := bufio.NewScanner(conn)
	read := func(want byte) string {
		t.Helper()
		for sc.Scan() {
			line := sc.Text()
			if len(line) >= 4 && line[3] == '-' {
				continue
			}
			if len(line) == 0 || line[0] != want {
				t.Fatalf("lmtp: got %q, want a %c reply", line, want)
			}
			return line
		}
		t.Fatalf("lmtp: connection closed waiting for a %c reply", want)
		return ""
	}
	read('2')
	for _, cmd := range []string{
		"LHLO test\r\n",
		"MAIL FROM:<sender@external.com>\r\n",
		"RCPT TO:<" + rcpt + ">\r\n",
	} {
		if _, err := io.WriteString(conn, cmd); err != nil {
			t.Fatal(err)
		}
		read('2')
	}
	if _, err := io.WriteString(conn, "DATA\r\n"); err != nil {
		t.Fatal(err)
	}
	read('3')
	if _, err := io.WriteString(conn, body+".\r\n"); err != nil {
		t.Fatal(err)
	}
	read('2')
	io.WriteString(conn, "QUIT\r\n") //nolint:errcheck
}

// formatPassdb adds the userdb field naming the driver: without it the session
// resolves a different index layout than the delivery wrote to.
type formatPassdb struct {
	user, pass, format string
}

func (s *formatPassdb) Authenticate(username, password, _, _ string) (*protocol.AuthResponse, error) {
	if username != s.user || password != s.pass {
		return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
	}
	return &protocol.AuthResponse{
		Result: protocol.AuthOK, Username: username, MailboxFormat: s.format,
	}, nil
}
