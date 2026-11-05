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

// ruleConn builds the server and client ends of one connection kind.
type ruleConn struct {
	name    string
	secured bool
	wrap    func(t *testing.T, srv, cli net.Conn, srvTLS, cliTLS *tls.Config) (net.Conn, net.Conn)
}

func proxyHeader(t *testing.T, version byte, tlv *tlvparse.PP2SSL) *proxyproto.Header {
	t.Helper()
	h := &proxyproto.Header{Version: version, Command: proxyproto.PROXY, TransportProtocol: proxyproto.TCPv4,
		SourceAddr:      &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 40000},
		DestinationAddr: &net.TCPAddr{IP: net.ParseIP("198.51.100.1"), Port: 143}}
	if tlv != nil {
		tlv, err := tlv.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if err := h.SetTLVs([]proxyproto.TLV{tlv}); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func viaProxy(version byte, tlv *tlvparse.PP2SSL) func(*testing.T, net.Conn, net.Conn, *tls.Config, *tls.Config) (net.Conn, net.Conn) {
	return func(t *testing.T, srv, cli net.Conn, _, _ *tls.Config) (net.Conn, net.Conn) {
		if _, err := proxyHeader(t, version, tlv).WriteTo(cli); err != nil {
			t.Fatal(err)
		}
		return proxyproto.NewConn(srv), cli
	}
}

var ruleConns = []ruleConn{
	{"open", false, func(_ *testing.T, srv, cli net.Conn, _, _ *tls.Config) (net.Conn, net.Conn) {
		return remoteConn{srv}, cli
	}},
	{"TLS", true, func(_ *testing.T, srv, cli net.Conn, s, c *tls.Config) (net.Conn, net.Conn) {
		return tls.Server(remoteConn{srv}, s), tls.Client(cli, c)
	}},
	{"local", true, func(_ *testing.T, srv, cli net.Conn, _, _ *tls.Config) (net.Conn, net.Conn) { return srv, cli }},
	{"PROXY v2 with SSL TLV", true, viaProxy(2, &tlvparse.PP2SSL{Client: tlvparse.PP2_BITFIELD_CLIENT_SSL,
		TLV: []proxyproto.TLV{{Type: proxyproto.PP2_SUBTYPE_SSL_VERSION, Value: []byte("TLSv1.3")}}})},
	{"PROXY v2 SSL TLV without the client bit", false, viaProxy(2, &tlvparse.PP2SSL{})},
	{"PROXY v2 without TLV", false, viaProxy(2, nil)},
	{"PROXY v1", false, viaProxy(1, nil)},
}

// ruleProto drives one protocol: what its capability list says, and a login
// with the right password.
type ruleProto struct {
	name    string
	run     func(net.Conn, *bufio.Reader, *tls.Config, Options) (*preamble, error)
	caps    func(t *testing.T, cli net.Conn, rd *bufio.Reader) string
	login   []string
	refusal string
	// hidden appears in caps only while cleartext is allowed; shown only while refused.
	hidden, shown []string
}

func readLinesUntil(t *testing.T, rd *bufio.Reader, end func(string) bool) string {
	t.Helper()
	var b strings.Builder
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v (so far %q)", err, b.String())
		}
		b.WriteString(line)
		if end(line) {
			return b.String()
		}
	}
}

var ruleProtos = []ruleProto{
	{name: "IMAP",
		run: func(c net.Conn, rd *bufio.Reader, x *tls.Config, o Options) (*preamble, error) {
			p, _, _, err := extractIMAPPreamble(c, rd, x, o, relayContext{})
			return p, err
		},
		caps: func(t *testing.T, _ net.Conn, rd *bufio.Reader) string {
			return readLinesUntil(t, rd, func(string) bool { return true })
		},
		login: []string{"a1 LOGIN alice secret"}, refusal: "a1 NO [PRIVACYREQUIRED] " + cleartextDisabledMsg,
		hidden: []string{" AUTH=PLAIN", " AUTH=LOGIN"}, shown: []string{" LOGINDISABLED"}},
	{name: "POP3",
		run: func(c net.Conn, rd *bufio.Reader, x *tls.Config, o Options) (*preamble, error) {
			p, _, _, err := extractPOP3Preamble(c, rd, x, o, relayContext{})
			return p, err
		},
		caps: func(t *testing.T, cli net.Conn, rd *bufio.Reader) string {
			readLinesUntil(t, rd, func(string) bool { return true })
			fmt.Fprintf(cli, "CAPA\r\n")
			return readLinesUntil(t, rd, func(l string) bool { return l == ".\r\n" })
		},
		login: []string{"USER alice", "PASS secret"}, refusal: "-ERR [AUTH] " + cleartextDisabledMsg,
		hidden: []string{"\r\nUSER\r\n", "PLAIN LOGIN"}},
	{name: "ManageSieve",
		run: func(c net.Conn, rd *bufio.Reader, x *tls.Config, o Options) (*preamble, error) {
			p, _, _, err := extractManageSievePreamble(c, rd, x, o, relayContext{})
			return p, err
		},
		caps: func(t *testing.T, _ net.Conn, rd *bufio.Reader) string {
			return readLinesUntil(t, rd, func(l string) bool { return strings.HasPrefix(l, "OK") })
		},
		login: []string{`AUTHENTICATE "PLAIN" "` + plainB64("alice", "secret") + `"`}, refusal: `NO "` + cleartextDisabledMsg,
		hidden: []string{`"SASL" "PLAIN LOGIN"`}},
}

// The rule across every protocol, connection kind, listener shape and switch:
// refused only when the switch is off, the listener offers STARTTLS and the
// connection is not secured. The capability list and the login agree with it.
func TestCleartextRuleMatrix(t *testing.T) {
	srvTLS, cliTLS := cleartextTLS(t)
	listeners := []struct {
		name     string
		starttls bool
		implicit bool
	}{{"STARTTLS", true, false}, {"no TLS", false, false}, {"implicit TLS", false, true}}
	for _, p := range ruleProtos {
		for _, c := range ruleConns {
			for _, l := range listeners {
				if l.implicit && c.name != "TLS" {
					continue // an implicit-TLS listener hands the preamble a TLS conn only
				}
				for _, allow := range []bool{false, true} {
					refused := !allow && l.starttls && !c.secured
					name := fmt.Sprintf("%s/%s/%s/allow=%v", p.name, c.name, l.name, allow)
					t.Run(name, func(t *testing.T) {
						var extTLS *tls.Config
						if l.starttls {
							extTLS = srvTLS
						}
						s0, c0 := pipePair(t)
						srv, cli := c.wrap(t, s0, c0, srvTLS, cliTLS)
						done := make(chan *preamble, 1)
						go func() {
							pre, _ := p.run(srv, bufio.NewReader(srv), extTLS, testOpts(Options{DisablePlainAuth: !allow}))
							done <- pre
						}()
						c0.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
						rd := bufio.NewReader(cli)
						caps := p.caps(t, cli, rd)
						for _, h := range p.hidden {
							if strings.Contains(caps, h) == refused {
								t.Errorf("%q present = %v, want %v:\n%s", h, refused, !refused, caps)
							}
						}
						for _, s := range p.shown {
							if strings.Contains(caps, s) != refused {
								t.Errorf("%q present = %v, want %v:\n%s", s, !refused, refused, caps)
							}
						}
						for _, line := range p.login {
							fmt.Fprintf(cli, "%s\r\n", line)
						}
						if refused {
							readReply(t, rd, p.refusal)
							select {
							case pre := <-done:
								t.Fatalf("credentials %+v passed on a refused line", pre)
							case <-time.After(100 * time.Millisecond):
							}
							return
						}
						go func() { _, _ = rd.ReadString(0) }()
						select {
						case pre := <-done:
							if pre == nil || pre.username != "alice" || pre.password != "secret" {
								t.Fatalf("got %+v, want alice's credentials", pre)
							}
						case <-time.After(3 * time.Second):
							t.Fatal("the login did not go through")
						}
					})
				}
			}
		}
	}
}

// The advisory's case: after STARTTLS the refused IMAP line advertises
// AUTH=PLAIN again and the same LOGIN goes through.
func TestIMAPLoginDisabledUntilSTARTTLS(t *testing.T) {
	srvTLS, cliTLS := cleartextTLS(t)
	srv, cli := pipePair(t)
	done := make(chan *preamble, 1)
	go func() {
		p, _, _, _ := extractIMAPPreamble(remoteConn{srv}, bufio.NewReader(srv), srvTLS, testOpts(Options{DisablePlainAuth: true}), relayContext{})
		done <- p
	}()
	cli.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	rd := bufio.NewReader(cli)
	if g := readLinesUntil(t, rd, func(string) bool { return true }); !strings.Contains(g, " LOGINDISABLED") || strings.Contains(g, "AUTH=PLAIN") {
		t.Fatalf("greeting before STARTTLS: %q", g)
	}
	fmt.Fprintf(cli, "a1 LOGIN alice secret\r\ns1 STARTTLS\r\n")
	readReply(t, rd, "a1 NO [PRIVACYREQUIRED]")
	readReply(t, rd, "s1 OK")
	tc := tls.Client(cli, cliTLS)
	trd := bufio.NewReader(tc)
	fmt.Fprintf(tc, "c1 CAPABILITY\r\n")
	if c := readReply(t, trd, "* CAPABILITY"); strings.Contains(c, "LOGINDISABLED") || !strings.Contains(c, "AUTH=PLAIN") {
		t.Fatalf("capability after STARTTLS: %q", c)
	}
	fmt.Fprintf(tc, "a2 LOGIN alice secret\r\n")
	select {
	case p := <-done:
		if p == nil || p.username != "alice" {
			t.Fatalf("after STARTTLS got %+v", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the login after STARTTLS did not go through")
	}
}
