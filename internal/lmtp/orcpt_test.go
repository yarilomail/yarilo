package lmtp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	goSmtp "github.com/emersion/go-smtp"

	"github.com/yarilomail/yarilo/v2/internal/sieve"
	fileindex "github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// replyRelay is the submission host the vacation reply goes to.
type replyRelay struct {
	mu   sync.Mutex
	sent []string // envelope recipients
}

func (r *replyRelay) NewSession(*goSmtp.Conn) (goSmtp.Session, error) {
	return &replySession{r: r}, nil
}

func (r *replyRelay) to() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.sent, ",")
}

type replySession struct{ r *replyRelay }

func (s *replySession) Mail(string, *goSmtp.MailOptions) error { return nil }
func (s *replySession) Rcpt(to string, _ *goSmtp.RcptOptions) error {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	s.r.sent = append(s.r.sent, to)
	return nil
}
func (s *replySession) Data(r io.Reader) error { _, err := io.Copy(io.Discard, r); return err }
func (s *replySession) Reset()                 {}
func (s *replySession) Logout() error          { return nil }

// orcptServer is an LMTP server whose alice@example.com runs vacation with
// sieve_vacation_use_original_recipient, replying through the returned relay.
func orcptServer(t *testing.T) (string, *replyRelay) {
	t.Helper()
	relay := &replyRelay{}
	rs := goSmtp.NewServer(relay)
	rs.Domain = "relay.test"
	rl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go rs.Serve(rl) //nolint:errcheck
	t.Cleanup(func() { rs.Close() })

	resolver := &mailbox.Resolver{Root: t.TempDir(), HomeTemplate: "%d/%n"}
	info := mustUserInfo(resolver, "alice@example.com")
	box := maildir.New().OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	box.Close() //nolint:errcheck
	st := sieve.NewScriptStore("fs", "", nil, nil)
	ctx := context.Background()
	if err := st.SaveScript(ctx, "alice@example.com", info.Home, "away", []byte(`require "vacation"; vacation "away";`)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetActive(ctx, "alice@example.com", info.Home, "away"); err != nil {
		t.Fatal(err)
	}
	scfg := config.Defaults().Sieve
	scfg.Enabled, scfg.VacationUseOriginalRecipient = true, true
	scfg.SubmissionHost, scfg.SubmissionSSL, scfg.SubmissionTimeout = rl.Addr().String(), "no", 5

	srv := New(testOpts(Options{Hostname: "lmtp.test", Config: config.LMTPProtocolConfig{ReadTimeout: 5, WriteTimeout: 5},
		Mailbox: maildir.New(), Index: fileindex.New(), Resolver: resolver, SieveEngine: sieve.New(scfg, nil, nil, nil)}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), relay
}

// transaction sends one message from sender to alice, naming only
// first@example.com in To, with param after the RCPT address.
func transaction(t *testing.T, conn net.Conn, sc *bufio.Scanner, sender, param string) {
	t.Helper()
	fmt.Fprintf(conn, "MAIL FROM:<%s>\r\n", sender)
	sc.Scan()
	fmt.Fprintf(conn, "RCPT TO:<alice@example.com>%s\r\n", param)
	sc.Scan()
	if !strings.HasPrefix(sc.Text(), "250") {
		t.Fatalf("RCPT answered %q", sc.Text())
	}
	fmt.Fprintf(conn, "DATA\r\n")
	sc.Scan()
	fmt.Fprintf(conn, "From: %s\r\nTo: first@example.com\r\nSubject: hi\r\n\r\nb\r\n.\r\n", sender)
	sc.Scan()
	if !strings.HasPrefix(sc.Text(), "250") {
		t.Fatalf("DATA answered %q", sc.Text())
	}
}

// An ORCPT from the relay reaches Sieve as the original recipient: a message
// naming only that address is answered with it and not without it (#2280).
func TestORCPTReachesSieve(t *testing.T) {
	for _, withORCPT := range []bool{true, false} {
		t.Run(fmt.Sprintf("orcpt=%v", withORCPT), func(t *testing.T) {
			addr, relay := orcptServer(t)
			conn, sc := dialLMTP(t, addr)
			sendLHLO(t, conn, sc)
			param, want := "", ""
			if withORCPT {
				param, want = " ORCPT=rfc822;first@example.com", "bob@ext.test"
			}
			transaction(t, conn, sc, "bob@ext.test", param)
			if got := relay.to(); got != want {
				t.Errorf("vacation reply sent to %q, want %q", got, want)
			}
		})
	}
}

// The next transaction on the connection does not inherit an ORCPT.
func TestORCPTIsPerTransaction(t *testing.T) {
	addr, relay := orcptServer(t)
	conn, sc := dialLMTP(t, addr)
	sendLHLO(t, conn, sc)
	transaction(t, conn, sc, "bob@ext.test", " ORCPT=rfc822;first@example.com")
	transaction(t, conn, sc, "carol@ext.test", "")
	if got := relay.to(); got != "bob@ext.test" {
		t.Errorf("vacation replies sent to %q, want only bob@ext.test", got)
	}
}
