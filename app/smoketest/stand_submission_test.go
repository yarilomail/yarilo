package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	goSmtp "github.com/emersion/go-smtp"

	"github.com/yarilomail/yarilo/v2/internal/auth/authtest"
	"github.com/yarilomail/yarilo/v2/internal/jmap"
	"github.com/yarilomail/yarilo/v2/internal/submission"
	"github.com/yarilomail/yarilo/v2/internal/submission/proxy"
	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// standTLS is a self-signed certificate for 127.0.0.1; the probe runs -insecure.
func standTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "stand"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

// lmtpRelay stands in for the deployment's MTA: it takes what submission
// relays and hands it to the stand's LMTP, as Postfix hands it to lmtp-login.
// It reads the LMTP address per message: the relay can start before LMTP does.
type lmtpRelay struct{ stand *localStand }

func (r lmtpRelay) NewSession(*goSmtp.Conn) (goSmtp.Session, error) {
	return &lmtpRelaySession{addr: r.stand.lmtpAddr}, nil
}

type lmtpRelaySession struct {
	addr  string
	from  string
	rcpts []string
}

func (s *lmtpRelaySession) Mail(from string, _ *goSmtp.MailOptions) error { s.from = from; return nil }
func (s *lmtpRelaySession) Rcpt(to string, _ *goSmtp.RcptOptions) error {
	s.rcpts = append(s.rcpts, to)
	return nil
}
func (s *lmtpRelaySession) Reset()        { s.from, s.rcpts = "", nil }
func (s *lmtpRelaySession) Logout() error { return nil }

func (s *lmtpRelaySession) Data(r io.Reader) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("tcp", s.addr, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck
	rd := bufio.NewReader(conn)
	expect := func(code string) error {
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return err
			}
			if !strings.HasPrefix(line, code) {
				return fmt.Errorf("lmtp answered %q, want %s", strings.TrimSpace(line), code)
			}
			if len(line) > 3 && line[3] == ' ' {
				return nil
			}
		}
	}
	steps := []struct{ send, want string }{
		{"", "220"}, {"LHLO relay.test", "250"}, {"MAIL FROM:<" + s.from + ">", "250"},
	}
	for _, rc := range s.rcpts {
		steps = append(steps, struct{ send, want string }{"RCPT TO:<" + rc + ">", "250"})
	}
	steps = append(steps, struct{ send, want string }{"DATA", "354"})
	for _, st := range steps {
		if st.send != "" {
			if _, err := fmt.Fprintf(conn, "%s\r\n", st.send); err != nil {
				return err
			}
		}
		if err := expect(st.want); err != nil {
			return err
		}
	}
	// An MTA stamps its own hop, as Postfix does on the deployment's relay.
	stamp := "Received: from mail.example.com by relay.test with ESMTP; " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n"
	text := strings.ReplaceAll(stamp+string(body), "\r\n.", "\r\n..")
	if !strings.HasSuffix(text, "\r\n") {
		text += "\r\n"
	}
	if _, err := fmt.Fprintf(conn, "%s.\r\n", text); err != nil {
		return err
	}
	for range s.rcpts {
		if err := expect("250"); err != nil {
			return err
		}
	}
	return nil
}

// withSubmission serves submission with STARTTLS, relaying through lmtpRelay
// into the stand's LMTP, and points the probe's SMTP flags at it.
func (s *localStand) withSubmission(t *testing.T) {
	t.Helper()
	srv := submission.New(submission.Options{
		Config:    config.SubmissionProtocolConfig{Hostname: "mail.example.com", MaxMsgSize: 1 << 20},
		TLSConfig: standTLS(t),
		AuthRelay: authtest.RelayToChain(t, standPassdb{}),
		Proxy:     proxy.New(s.relay(t), "mail.example.com"),
	})
	ln, subPort := s.listen(t)
	go func() { _ = srv.Serve(ln, nil) }()
	setFlag(t, flagSMTPHost, "127.0.0.1")
	setFlag(t, flagSMTPSubPort, subPort)
	setFlag(t, flagInsecure, true)
	setFlag(t, flagSMTPSub, true)
	setFlag(t, flagSMTPSubSend, true)
}

// relay starts the MTA stand-in once and names it as submission's relay.
func (s *localStand) relay(t *testing.T) config.RelayConfig {
	t.Helper()
	if s.relayCfg != nil {
		return *s.relayCfg
	}
	rln, _ := s.listen(t)
	relay := goSmtp.NewServer(lmtpRelay{stand: s})
	relay.Domain = "relay.test"
	go func() { _ = relay.Serve(rln) }()
	host, portStr, _ := net.SplitHostPort(rln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	s.relayCfg = &config.RelayConfig{Host: host, Port: port, SSL: "no", ConnectTimeout: 5, CommandTimeout: 10}
	return *s.relayCfg
}

// The JMAP rows that send: EmailSubmission, Email/set with header:* sent, and
// an Identity sent through, each through the relay into the INBOX.
func TestJMAPSendingRowsAgainstTheServers(t *testing.T) {
	s := startStand(t)
	s.withIMAP(t)
	s.withLMTP(t)
	s.withSubmission(t)
	s.withJMAP(t, func(o *jmap.Options) {
		o.Submission = &jmap.Submission{Sender: proxy.New(s.relay(t), "mail.example.com"), MaxSize: 1 << 20,
			Hostname: "mail.example.com", TTL: time.Hour}
	})
	rows := []struct {
		name string
		fn   func() error
	}{
		{"email submission", checkJMAPSubmission},
		{"create and send", checkJMAPCreateAndSend},
		{"identity", checkJMAPIdentity},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			if err := r.fn(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The submission rows: EHLO, STARTTLS and AUTH, then a message sent through
// the relay and found in the sender's INBOX over IMAP.
func TestSubmissionRowsAgainstTheServer(t *testing.T) {
	s := startStand(t)
	s.withIMAP(t)
	s.withLMTP(t)
	s.withSubmission(t)
	if err := checkSMTPSubmission(); err != nil {
		t.Fatalf("submission EHLO+STARTTLS: %v", err)
	}
	if err := checkSMTPSubmissionSend(); err != nil {
		t.Fatalf("submission send: %v", err)
	}
}
