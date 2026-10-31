package submission

import (
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	goSmtp "github.com/emersion/go-smtp"

	"github.com/yarilomail/yarilo/v2/internal/auth/authtest"
	"github.com/yarilomail/yarilo/v2/internal/submission/proxy"
	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// recordingRelay refuses the sender or recipient refused@relay.test and a body
// holding REJECT, records what it accepts and counts its open connections.
type recordingRelay struct {
	open atomic.Int32
	mu   sync.Mutex
	sent []relayed
}

type relayed struct {
	from  string
	rcpts []string
}

func (r *recordingRelay) NewSession(*goSmtp.Conn) (goSmtp.Session, error) {
	r.open.Add(1)
	return &recordingSession{relay: r}, nil
}

type recordingSession struct {
	relay *recordingRelay
	cur   relayed
}

func (s *recordingSession) Mail(from string, _ *goSmtp.MailOptions) error {
	if from == "refused@relay.test" {
		return &goSmtp.SMTPError{Code: 553, EnhancedCode: goSmtp.EnhancedCode{5, 7, 1}, Message: "Sender refused here"}
	}
	s.cur = relayed{from: from}
	return nil
}

func (s *recordingSession) Rcpt(to string, _ *goSmtp.RcptOptions) error {
	if to == "refused@relay.test" {
		return &goSmtp.SMTPError{Code: 550, EnhancedCode: goSmtp.EnhancedCode{5, 1, 1}, Message: "Recipient refused here"}
	}
	s.cur.rcpts = append(s.cur.rcpts, to)
	return nil
}

func (s *recordingSession) Data(r io.Reader) error {
	b, _ := io.ReadAll(r)
	if strings.Contains(string(b), "REJECT") {
		return &goSmtp.SMTPError{Code: 554, EnhancedCode: goSmtp.EnhancedCode{5, 7, 1}, Message: "Content refused here"}
	}
	s.relay.mu.Lock()
	s.relay.sent = append(s.relay.sent, s.cur)
	s.relay.mu.Unlock()
	return nil
}

func (s *recordingSession) Reset()        { s.cur = relayed{} }
func (s *recordingSession) Logout() error { s.relay.open.Add(-1); return nil }

// startRelayedSubmission runs a submission server whose relay is a recordingRelay.
func startRelayedSubmission(t *testing.T) (*recordingRelay, string) {
	t.Helper()
	relay := &recordingRelay{}
	rln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rsrv := goSmtp.NewServer(relay)
	rsrv.Domain = "relay.test"
	go rsrv.Serve(rln) //nolint:errcheck
	t.Cleanup(func() { rsrv.Close() })
	host, port, _ := net.SplitHostPort(rln.Addr().String())
	p, _ := strconv.Atoi(port)
	cfg := config.RelayConfig{Host: host, Port: p, SSL: "no", ConnectTimeout: 5, CommandTimeout: 5}
	return relay, startSubmissionWithRelay(t, cfg)
}

func startSubmissionWithRelay(t *testing.T, cfg config.RelayConfig) string {
	t.Helper()
	srv := New(Options{
		Config:    config.SubmissionProtocolConfig{Hostname: "mx.example.com", MaxMsgSize: 1 << 20},
		AuthRelay: authtest.RelayTo(t, authtest.PlainOnly(stubAuth{})),
		Proxy:     proxy.New(cfg, "mx.example.com"),
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln, nil) }()
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

func loggedIn(t *testing.T, addr string) *goSmtp.Client {
	t.Helper()
	c := dialSMTP(t, addr)
	if err := c.Auth(plainAuth("alice@example.com", "secret")); err != nil {
		t.Fatalf("AUTH: %v", err)
	}
	return c
}

// wantReply checks err is the relay's reply as it is: code, enhanced code, text.
func wantReply(t *testing.T, what string, err error, code int, enh goSmtp.EnhancedCode, text string) {
	t.Helper()
	var se *goSmtp.SMTPError
	if !errors.As(err, &se) || se.Code != code || se.EnhancedCode != enh || se.Message != text {
		t.Errorf("%s: %v, want %d %v %s", what, err, code, enh, text)
	}
}

func data(c *goSmtp.Client, body string) error {
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, body); err != nil {
		return err
	}
	return w.Close()
}

// waitClosed fails unless every relay connection is closed within a second.
func waitClosed(t *testing.T, relay *recordingRelay) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for relay.open.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d relay connections still open", relay.open.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// One recipient of three refused by the relay: its RCPT carries the relay's
// reply, and the other two receive the message.
func TestARecipientTheRelayRefusesLeavesTheOthers(t *testing.T) {
	relay, addr := startRelayedSubmission(t)
	c := loggedIn(t, addr)
	defer c.Close()
	if err := c.Mail("alice@example.com", nil); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	if err := c.Rcpt("bob@relay.test", nil); err != nil {
		t.Fatalf("RCPT bob: %v", err)
	}
	wantReply(t, "RCPT refused", c.Rcpt("refused@relay.test", nil), 550, goSmtp.EnhancedCode{5, 1, 1}, "Recipient refused here")
	if err := c.Rcpt("carol@relay.test", nil); err != nil {
		t.Fatalf("RCPT carol: %v", err)
	}
	if err := data(c, "Subject: x\r\n\r\nx\r\n"); err != nil {
		t.Fatalf("DATA: %v", err)
	}
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if len(relay.sent) != 1 || strings.Join(relay.sent[0].rcpts, ",") != "bob@relay.test,carol@relay.test" {
		t.Errorf("relay received %+v, want one message to bob and carol", relay.sent)
	}
}

// The relay's refusal of MAIL or DATA is the client's reply as it is, and the
// relay connection is closed after either.
func TestTheRelaysRefusalOfMailOrDataReachesTheClient(t *testing.T) {
	cases := []struct {
		name string
		from string
		body string
		code int
		enh  goSmtp.EnhancedCode
		text string
	}{
		{"MAIL", "refused@relay.test", "", 553, goSmtp.EnhancedCode{5, 7, 1}, "Sender refused here"},
		{"DATA", "alice@example.com", "Subject: x\r\n\r\nREJECT\r\n", 554, goSmtp.EnhancedCode{5, 7, 1}, "Content refused here"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			relay, addr := startRelayedSubmission(t)
			c := loggedIn(t, addr)
			defer c.Close()
			err := c.Mail(tc.from, nil)
			if tc.body != "" {
				if err != nil {
					t.Fatalf("MAIL: %v", err)
				}
				if err := c.Rcpt("bob@relay.test", nil); err != nil {
					t.Fatalf("RCPT: %v", err)
				}
				err = data(c, tc.body)
			}
			wantReply(t, tc.name, err, tc.code, tc.enh, tc.text)
			waitClosed(t, relay)
		})
	}
}

// RSET ends the relay transaction: the next one carries only its own recipients.
func TestRSETEndsTheRelayTransaction(t *testing.T) {
	relay, addr := startRelayedSubmission(t)
	c := loggedIn(t, addr)
	defer c.Close()
	if err := c.Mail("alice@example.com", nil); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	if err := c.Rcpt("first@relay.test", nil); err != nil {
		t.Fatalf("RCPT: %v", err)
	}
	if err := c.Reset(); err != nil {
		t.Fatalf("RSET: %v", err)
	}
	waitClosed(t, relay)
	if err := c.Mail("alice@example.com", nil); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	if err := c.Rcpt("second@relay.test", nil); err != nil {
		t.Fatalf("RCPT: %v", err)
	}
	if err := data(c, "Subject: x\r\n\r\nx\r\n"); err != nil {
		t.Fatalf("DATA: %v", err)
	}
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if len(relay.sent) != 1 || strings.Join(relay.sent[0].rcpts, ",") != "second@relay.test" {
		t.Errorf("relay received %+v, want one message to second only", relay.sent)
	}
}

// A client dropping mid-transaction leaves no relay connection open.
func TestADroppedClientClosesTheRelayConnection(t *testing.T) {
	relay, addr := startRelayedSubmission(t)
	c := loggedIn(t, addr)
	if err := c.Mail("alice@example.com", nil); err != nil {
		t.Fatalf("MAIL: %v", err)
	}
	if err := c.Rcpt("bob@relay.test", nil); err != nil {
		t.Fatalf("RCPT: %v", err)
	}
	if n := relay.open.Load(); n != 1 {
		t.Fatalf("%d relay connections during the transaction, want 1", n)
	}
	c.Close()
	waitClosed(t, relay)
}

// A relay that cannot be reached fails MAIL with 451, holding nothing open.
func TestAnUnreachableRelayFailsMail(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	p, _ := strconv.Atoi(port)
	addr := startSubmissionWithRelay(t, config.RelayConfig{Host: host, Port: p, SSL: "no", ConnectTimeout: 5, CommandTimeout: 5})
	c := loggedIn(t, addr)
	defer c.Close()
	wantReply(t, "MAIL", c.Mail("alice@example.com", nil), 451, goSmtp.EnhancedCode{4, 4, 0}, "Failed to connect to upstream MTA")
	if err := c.Rcpt("bob@relay.test", nil); err == nil {
		t.Error("RCPT after a failed MAIL was accepted")
	}
}
