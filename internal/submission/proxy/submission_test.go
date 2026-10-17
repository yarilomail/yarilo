package proxy

import (
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/emersion/go-sasl"
	goSmtp "github.com/emersion/go-smtp"

	"github.com/yarilomail/yarilo/pkg/config"
)

func TestRelayAuth(t *testing.T) {
	cases := []struct {
		name                 string
		cfg                  config.RelayConfig
		login                string
		wantAuthcid, wantFor string
	}{
		{"no relay user: no AUTH", config.RelayConfig{}, "u@d.test", "", ""},
		{"a fixed relay user", config.RelayConfig{User: "svc"}, "u@d.test", "svc", ""},
		{"%u is the session's user", config.RelayConfig{User: "%u"}, "u@d.test", "u@d.test", ""},
		{"%{user} too", config.RelayConfig{User: "%{user}"}, "u@d.test", "u@d.test", ""},
		{"a master acting for the user", config.RelayConfig{User: "%u", MasterUser: "master"}, "u@d.test", "master", "u@d.test"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			authcid, authzid := New(c.cfg, "h").relayAuth(c.login)
			if authcid != c.wantAuthcid || authzid != c.wantFor {
				t.Errorf("relayAuth = (%q, %q), want (%q, %q)", authcid, authzid, c.wantAuthcid, c.wantFor)
			}
		})
	}
}

// relaySeen records the PLAIN identity a relay was given.
type relaySeen struct {
	mu                           sync.Mutex
	identity, username, password string
}

type authRelay struct{ seen *relaySeen }

func (b authRelay) NewSession(*goSmtp.Conn) (goSmtp.Session, error) {
	return &authRelaySession{b.seen}, nil
}

type authRelaySession struct{ seen *relaySeen }

func (s *authRelaySession) AuthMechanisms() []string { return []string{sasl.Plain} }
func (s *authRelaySession) Auth(string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(identity, username, password string) error {
		s.seen.mu.Lock()
		defer s.seen.mu.Unlock()
		s.seen.identity, s.seen.username, s.seen.password = identity, username, password
		return nil
	}), nil
}
func (s *authRelaySession) Mail(string, *goSmtp.MailOptions) error { return nil }
func (s *authRelaySession) Rcpt(string, *goSmtp.RcptOptions) error { return nil }
func (s *authRelaySession) Data(r io.Reader) error                 { _, _ = io.ReadAll(r); return nil }
func (s *authRelaySession) Reset()                                 {}
func (s *authRelaySession) Logout() error                          { return nil }

// The relay is told the master and, as authzid, the session's user.
func TestSendLogsInAsTheMasterForTheUser(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	seen := &relaySeen{}
	srv := goSmtp.NewServer(authRelay{seen})
	srv.Domain = "relay.test"
	srv.AllowInsecureAuth = true
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(port)
	cfg := config.RelayConfig{Host: host, Port: p, SSL: "no", User: "%u", MasterUser: "master", Password: "pw",
		ConnectTimeout: 5, CommandTimeout: 5}

	err = New(cfg, "mx.test").Send("u@d.test", []string{"x@e.test"}, strings.NewReader("Subject: x\r\n\r\nx\r\n"), nil, "u@d.test")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	seen.mu.Lock()
	defer seen.mu.Unlock()
	if seen.identity != "u@d.test" || seen.username != "master" || seen.password != "pw" {
		t.Errorf("relay saw PLAIN (%q, %q, %q), want (u@d.test, master, pw)", seen.identity, seen.username, seen.password)
	}
}
