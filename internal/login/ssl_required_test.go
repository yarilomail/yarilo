package login

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	proxyproto "github.com/pires/go-proxyproto"
	"github.com/pires/go-proxyproto/tlvparse"
)

// viaProxyTLS sends a PROXY v2 header whose PP2_TYPE_SSL TLV says the client
// used TLS to the proxy.
func viaProxyTLS(t *testing.T, srv, cli net.Conn, _, _ *tls.Config) (net.Conn, net.Conn) {
	h := &proxyproto.Header{Version: 2, Command: proxyproto.PROXY, TransportProtocol: proxyproto.TCPv4,
		SourceAddr:      &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 40000},
		DestinationAddr: &net.TCPAddr{IP: net.ParseIP("198.51.100.1"), Port: 143}}
	tlv, err := tlvparse.PP2SSL{Client: tlvparse.PP2_BITFIELD_CLIENT_SSL,
		TLV: []proxyproto.TLV{{Type: proxyproto.PP2_SUBTYPE_SSL_VERSION, Value: []byte("TLSv1.3")}}}.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.SetTLVs([]proxyproto.TLV{tlv}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.WriteTo(cli); err != nil {
		t.Fatal(err)
	}
	return proxyproto.NewConn(remoteConn{srv}), cli
}

// Under ssl_mode required only TLS, a local peer and a PROXY header with
// PP2_CLIENT_SSL are secured; a trusted peer and a plain PROXY header are not.
var requiredConns = []ruleConn{
	{name: "open", wrap: openConn},
	{name: "TLS", secured: true, wrap: func(_ *testing.T, srv, cli net.Conn, s, c *tls.Config) (net.Conn, net.Conn) {
		return tls.Server(remoteConn{srv}, s), tls.Client(cli, c)
	}},
	{name: "local", secured: true, wrap: func(_ *testing.T, srv, cli net.Conn, _, _ *tls.Config) (net.Conn, net.Conn) { return srv, cli }},
	{name: "trusted-net peer", trusted: true, wrap: openConn},
	{name: "PROXY v2 with PP2_CLIENT_SSL", secured: true, wrap: viaProxyTLS},
	{name: "PROXY v2 without it", wrap: viaProxy(2)},
	{name: "PROXY v1", wrap: viaProxy(1)},
}

// requiredProto: the capability list, a SASL login with the right password and
// what refuses it, and the log line of that refusal.
type requiredProto struct {
	name    string
	run     func(net.Conn, *bufio.Reader, *tls.Config, Options, relayContext) (*preamble, error)
	caps    func(t *testing.T, cli net.Conn, rd *bufio.Reader) string
	login   []string
	refusal string
	// mechs are capability fragments present only while authentication is offered.
	mechs []string
}

var requiredProtos = []requiredProto{
	{name: "IMAP",
		run: func(c net.Conn, rd *bufio.Reader, x *tls.Config, o Options, rc relayContext) (*preamble, error) {
			p, _, _, err := extractIMAPPreamble(c, rd, x, o, rc)
			return p, err
		},
		caps: func(t *testing.T, _ net.Conn, rd *bufio.Reader) string {
			return readLinesUntil(t, rd, func(string) bool { return true })
		},
		login:   []string{"a1 AUTHENTICATE PLAIN " + plainB64("alice", "secret")},
		refusal: "a1 NO [PRIVACYREQUIRED] " + sslRequiredMsg,
		mechs:   []string{"AUTH=SCRAM-SHA-256"}},
	{name: "POP3",
		run: func(c net.Conn, rd *bufio.Reader, x *tls.Config, o Options, rc relayContext) (*preamble, error) {
			p, _, _, err := extractPOP3Preamble(c, rd, x, o, rc)
			return p, err
		},
		caps: func(t *testing.T, cli net.Conn, rd *bufio.Reader) string {
			readLinesUntil(t, rd, func(string) bool { return true })
			fmt.Fprintf(cli, "CAPA\r\n")
			return readLinesUntil(t, rd, func(l string) bool { return l == ".\r\n" })
		},
		login:   []string{"AUTH PLAIN " + plainB64("alice", "secret")},
		refusal: "-ERR [AUTH] " + sslRequiredMsg,
		mechs:   []string{"\r\nSASL ", "SCRAM-SHA-256"}},
	{name: "Submission",
		run: func(c net.Conn, rd *bufio.Reader, x *tls.Config, o Options, rc relayContext) (*preamble, error) {
			p, _, _, err := extractSubmissionPreamble(c, rd, x, o, rc)
			return p, err
		},
		caps: func(t *testing.T, cli net.Conn, rd *bufio.Reader) string {
			readLinesUntil(t, rd, func(string) bool { return true })
			fmt.Fprintf(cli, "EHLO client\r\n")
			return readLinesUntil(t, rd, func(l string) bool { return strings.HasPrefix(l, "250 ") })
		},
		login:   []string{"AUTH PLAIN " + plainB64("alice", "secret")},
		refusal: "530 5.7.0 TLS required.",
		mechs:   []string{"250-AUTH", "SCRAM-SHA-256"}},
	{name: "ManageSieve",
		run: func(c net.Conn, rd *bufio.Reader, x *tls.Config, o Options, rc relayContext) (*preamble, error) {
			p, _, _, err := extractManageSievePreamble(c, rd, x, o, rc)
			return p, err
		},
		caps: func(t *testing.T, _ net.Conn, rd *bufio.Reader) string {
			return readLinesUntil(t, rd, func(l string) bool { return strings.HasPrefix(l, "OK") })
		},
		login:   []string{`AUTHENTICATE "PLAIN" "` + plainB64("alice", "secret") + `"`},
		refusal: `NO (ENCRYPT-NEEDED) "` + sslRequiredMsg + `"`,
		mechs:   []string{`"SASL"`, "SCRAM-SHA-256"}},
}

// ssl_mode required across every protocol and connection kind: before a
// secured connection nothing is offered, SCRAM included, and every login is
// refused with the required reply; a secured one logs in.
func TestSSLRequiredMatrix(t *testing.T) {
	srvTLS, cliTLS := cleartextTLS(t)
	_, trusted, _ := net.ParseCIDR("192.0.2.0/24")
	for _, p := range requiredProtos {
		for _, c := range requiredConns {
			refused := !c.secured
			t.Run(p.name+"/"+c.name, func(t *testing.T) {
				logs := captureLog(t)
				opts := Options{DisablePlainAuth: true, SSLRequired: true}
				if c.trusted {
					opts.XClientNets = []*net.IPNet{trusted}
				}
				rc := relayContext{dial: relayTo(t, relayService(t, []string{"SCRAM-SHA-256"})), sessionID: "s1"}
				s0, c0 := pipePair(t)
				srv, cli := c.wrap(t, s0, c0, srvTLS, cliTLS)
				done := make(chan *preamble, 1)
				go func() {
					pre, _ := p.run(srv, bufio.NewReader(srv), srvTLS, testOpts(opts), rc)
					done <- pre
				}()
				c0.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
				rd := bufio.NewReader(cli)
				caps := p.caps(t, cli, rd)
				for _, m := range p.mechs {
					if got := strings.Contains(caps, m); got == refused {
						t.Errorf("%q present = %v, want %v:\n%s", m, got, !refused, caps)
					}
				}
				if p.name == "IMAP" && strings.Contains(caps, "LOGINDISABLED") != refused {
					t.Errorf("LOGINDISABLED present = %v, want %v:\n%s", !refused, refused, caps)
				}
				for _, line := range p.login {
					fmt.Fprintf(cli, "%s\r\n", line)
				}
				if refused {
					readReply(t, rd, p.refusal)
					select {
					case pre := <-done:
						t.Fatalf("credentials %+v passed before TLS", pre)
					case <-time.After(100 * time.Millisecond):
					}
					if p.name != "Submission" && logs.count("Login failed: SSL required for authentication") == 0 {
						t.Error("a refusal under ssl_mode required wrote no log line")
					}
					return
				}
				go func() { _, _ = rd.ReadString(0) }()
				select {
				case pre := <-done:
					if pre == nil || pre.username != "alice" {
						t.Fatalf("got %+v, want alice's credentials", pre)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("the login on a secured connection did not go through")
				}
			})
		}
	}
}

// Under required the SMTP proxy answers every command but EHLO, STARTTLS,
// NOOP and QUIT with 530 until TLS (RFC 3207 §4).
func TestSSLRequiredSMTPCommandsBeforeTLS(t *testing.T) {
	srvTLS, _ := cleartextTLS(t)
	for _, tc := range []struct {
		line, want string
	}{
		{"HELO client", "530 5.7.0 TLS required."},
		{"RSET", "530 5.7.0 TLS required."},
		{"XCLIENT ADDR=192.0.2.9", "530 5.7.0 TLS required."},
		{"MAIL FROM:<a@b>", "530 5.7.0 TLS required."},
		{"NOOP", "250 OK"},
		{"STARTTLS", "220 "},
	} {
		t.Run(tc.line, func(t *testing.T) {
			srv, cli := pipePair(t)
			go func() {
				_, _, _, _ = extractSubmissionPreamble(remoteConn{srv}, bufio.NewReader(srv), srvTLS,
					testOpts(Options{DisablePlainAuth: true, SSLRequired: true, XClient: true}), relayContext{})
			}()
			cli.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
			rd := bufio.NewReader(cli)
			readLinesUntil(t, rd, func(string) bool { return true })
			fmt.Fprintf(cli, "%s\r\n", tc.line)
			if got := readReply(t, rd, tc.want[:3]); !strings.HasPrefix(got, tc.want) {
				t.Errorf("%s answered %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

// LOGIN and USER/PASS carry the password itself: under required they are
// refused by the cleartext rule, with its text and its log line, as the reference does.
func TestSSLRequiredPasswordCommandsUseTheCleartextRefusal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		run   func(net.Conn, *bufio.Reader, *tls.Config, Options) error
		lines []string
		want  string
	}{
		{"IMAP LOGIN", func(c net.Conn, rd *bufio.Reader, x *tls.Config, o Options) error {
			_, _, _, err := extractIMAPPreamble(c, rd, x, o, relayContext{})
			return err
		}, []string{"a1 LOGIN alice secret"}, "a1 NO [PRIVACYREQUIRED] " + cleartextDisabledMsg},
		{"POP3 USER", func(c net.Conn, rd *bufio.Reader, x *tls.Config, o Options) error {
			_, _, _, err := extractPOP3Preamble(c, rd, x, o, relayContext{})
			return err
		}, []string{"USER alice"}, "-ERR [AUTH] " + cleartextDisabledMsg},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLog(t)
			srvTLS, _ := cleartextTLS(t)
			srv, cli := pipePair(t)
			go func() {
				_ = tc.run(remoteConn{srv}, bufio.NewReader(srv), srvTLS, testOpts(Options{DisablePlainAuth: true, SSLRequired: true}))
			}()
			cli.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
			rd := bufio.NewReader(cli)
			readLinesUntil(t, rd, func(string) bool { return true })
			for _, l := range tc.lines {
				fmt.Fprintf(cli, "%s\r\n", l)
			}
			readReply(t, rd, tc.want)
			if logs.count("Login failed: Cleartext authentication disabled") == 0 {
				t.Error("no cleartext refusal logged")
			}
		})
	}
}
