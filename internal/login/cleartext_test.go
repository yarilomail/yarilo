package login

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

func cleartextTLS(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "login-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}},
		&tls.Config{InsecureSkipVerify: true} //nolint:gosec // test peer
}

// remoteConn makes a loopback test connection look like a remote client.
type remoteConn struct{ net.Conn }

func (remoteConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 40000}
}

// cleartextCase drives one protocol: refuse before STARTTLS, allow after.
type cleartextCase struct {
	name, refused string
	run           func(srv net.Conn, rd *bufio.Reader, extTLS *tls.Config, opts Options) (*preamble, error)
	before        []string // lines up to the login attempt
	login         []string // the login, sent before and after STARTTLS
	starttls      string
	tlsOK         string   // the reply that hands the line to TLS
	afterTLS      []string // greeting lines to resend after STARTTLS (EHLO)
}

func imapPLAIN() string              { return base64PLAIN("alice", "secret") }
func base64PLAIN(u, p string) string { return plainB64(u, p) }

var cleartextCases = []cleartextCase{
	{name: "IMAP LOGIN", refused: "a1 NO [PRIVACYREQUIRED]",
		run: func(c net.Conn, rd *bufio.Reader, x *tls.Config, o Options) (*preamble, error) {
			p, _, _, err := extractIMAPPreamble(c, rd, x, o, relayContext{})
			return p, err
		},
		login: []string{"a1 LOGIN alice secret"}, starttls: "s1 STARTTLS", tlsOK: "s1 OK"},
	{name: "IMAP AUTHENTICATE PLAIN", refused: "a1 NO [PRIVACYREQUIRED]",
		run: func(c net.Conn, rd *bufio.Reader, x *tls.Config, o Options) (*preamble, error) {
			p, _, _, err := extractIMAPPreamble(c, rd, x, o, relayContext{})
			return p, err
		},
		login: []string{"a1 AUTHENTICATE PLAIN " + imapPLAIN()}, starttls: "s1 STARTTLS", tlsOK: "s1 OK"},
	{name: "POP3 USER", refused: "-ERR [AUTH]",
		run: func(c net.Conn, rd *bufio.Reader, x *tls.Config, o Options) (*preamble, error) {
			p, _, _, err := extractPOP3Preamble(c, rd, x, o, relayContext{})
			return p, err
		},
		login: []string{"USER alice", "PASS secret"}, starttls: "STLS", tlsOK: "+OK"},
	{name: "SMTP AUTH PLAIN", refused: "523 5.7.10",
		run: func(c net.Conn, rd *bufio.Reader, x *tls.Config, o Options) (*preamble, error) {
			p, _, _, err := extractSubmissionPreamble(c, rd, x, o, relayContext{})
			return p, err
		},
		before: []string{"EHLO client"}, login: []string{"AUTH PLAIN " + imapPLAIN()},
		starttls: "STARTTLS", tlsOK: "220 ", afterTLS: []string{"EHLO client"}},
	{name: "ManageSieve AUTHENTICATE PLAIN", refused: `NO "Cleartext authentication disallowed`,
		run: func(c net.Conn, rd *bufio.Reader, x *tls.Config, o Options) (*preamble, error) {
			p, _, _, err := extractManageSievePreamble(c, rd, x, o)
			return p, err
		},
		login: []string{`AUTHENTICATE "PLAIN" "` + imapPLAIN() + `"`}, starttls: "STARTTLS", tlsOK: "OK "},
}

// readReply reads lines until one starts with want or with a final status.
func readReply(t *testing.T, rd *bufio.Reader, want string) string {
	t.Helper()
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("waiting for %q: %v", want, err)
		}
		if strings.HasPrefix(line, want) {
			return line
		}
	}
}

// With cleartext off, PLAIN/LOGIN on an open line is refused with the
// protocol's code and yields no credentials; after STARTTLS it goes through.
func TestCleartextLoginIsRefusedUntilTLS(t *testing.T) {
	srvTLS, cliTLS := cleartextTLS(t)
	for _, tc := range cleartextCases {
		t.Run(tc.name, func(t *testing.T) {
			srv, cli := pipePair(t)
			done := make(chan *preamble, 1)
			go func() {
				p, _ := tc.run(remoteConn{srv}, bufio.NewReader(srv), srvTLS, Options{DisablePlainAuth: true})
				done <- p
			}()
			cli.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
			crd := bufio.NewReader(cli)
			send := func(lines []string) {
				for _, l := range lines {
					fmt.Fprintf(cli, "%s\r\n", l)
				}
			}
			send(tc.before)
			send(tc.login[:1])
			readReply(t, crd, tc.refused)
			select {
			case p := <-done:
				t.Fatalf("the proxy returned credentials %+v before TLS", p)
			case <-time.After(100 * time.Millisecond):
			}

			send([]string{tc.starttls})
			readReply(t, crd, tc.tlsOK)
			tlsCli := tls.Client(cli, cliTLS)
			if err := tlsCli.Handshake(); err != nil {
				t.Fatalf("STARTTLS handshake: %v", err)
			}
			for _, l := range append(tc.afterTLS, tc.login...) {
				fmt.Fprintf(tlsCli, "%s\r\n", l)
			}
			go func() { _, _ = bufio.NewReader(tlsCli).ReadString(0) }()
			select {
			case p := <-done:
				if p == nil || p.username != "alice" {
					t.Fatalf("after STARTTLS got %+v, want alice's credentials", p)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("after STARTTLS the login did not go through")
			}
		})
	}
}

// Implicit TLS and a local peer are secured: nothing is refused.
func TestCleartextLoginOnASecuredLine(t *testing.T) {
	srvTLS, cliTLS := cleartextTLS(t)
	for _, tc := range []struct {
		name string
		wrap func(srv, cli net.Conn) (net.Conn, net.Conn)
	}{
		{"implicit TLS", func(srv, cli net.Conn) (net.Conn, net.Conn) {
			return tls.Server(remoteConn{srv}, srvTLS), tls.Client(cli, cliTLS)
		}},
		{"local peer", func(srv, cli net.Conn) (net.Conn, net.Conn) { return srv, cli }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s0, c0 := pipePair(t)
			srv, cli := tc.wrap(s0, c0)
			done := make(chan *preamble, 1)
			go func() {
				p, _, _, _ := extractIMAPPreamble(srv, bufio.NewReader(srv), nil, Options{DisablePlainAuth: true}, relayContext{})
				done <- p
			}()
			cli.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
			fmt.Fprintf(cli, "a1 LOGIN alice secret\r\n")
			go func() { _, _ = bufio.NewReader(cli).ReadString(0) }()
			select {
			case p := <-done:
				if p == nil || p.username != "alice" {
					t.Fatalf("got %+v, want alice's credentials", p)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the login did not go through")
			}
		})
	}
}
