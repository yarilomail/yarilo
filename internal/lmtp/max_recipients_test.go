package lmtp

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func TestTheRecipientLimitIsAdvertisedAndHeld(t *testing.T) {
	srv := New(Options{
		Hostname: "lmtp.test",
		Config:   config.LMTPProtocolConfig{ReadTimeout: 5, WriteTimeout: 5, MaxRecipients: 2},
		Mailbox:  maildir.New(),
		Index:    file.New(),
		Resolver: &mailbox.Resolver{Root: t.TempDir(), HomeTemplate: "%d/%n"},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = srv.Serve(ln) }()

	conn, sc := dialLMTP(t, ln.Addr().String())
	fmt.Fprintf(conn, "LHLO postfix.test\r\n")
	advertised := false
	for sc.Scan() {
		if strings.Contains(sc.Text(), "LIMITS RCPTMAX=2") {
			advertised = true
		}
		if strings.HasPrefix(sc.Text(), "250 ") {
			break
		}
	}
	if !advertised {
		t.Error("LHLO does not advertise LIMITS RCPTMAX=2")
	}
	fmt.Fprintf(conn, "MAIL FROM:<s@x>\r\n")
	sc.Scan()
	var replies []string
	for _, r := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		fmt.Fprintf(conn, "RCPT TO:<%s>\r\n", r)
		sc.Scan()
		replies = append(replies, sc.Text())
	}
	if !strings.HasPrefix(replies[2], "452 4.5.3") {
		t.Fatalf("third RCPT = %q, want 452 4.5.3 (replies %q)", replies[2], replies)
	}
}
