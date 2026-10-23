package login

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	authclient "github.com/yarilomail/yarilo/internal/auth/client"
	"github.com/yarilomail/yarilo/internal/auth/protocol"
)

// recordingPolicy answers every check with decision and keeps the requests.
type recordingPolicy struct {
	mu       sync.Mutex
	decision protocol.PolicyDecision
	seen     []protocol.PolicyRequest
}

func (p *recordingPolicy) CheckBefore(_ context.Context, r protocol.PolicyRequest) (protocol.PolicyDecision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, r)
	return p.decision, nil
}

func (p *recordingPolicy) CheckAfter(_ context.Context, r protocol.PolicyRequest, _, _ bool) (protocol.PolicyDecision, error) {
	return protocol.PolicyDecision{Continue: true}, nil
}

func (p *recordingPolicy) ReportAfter(context.Context, protocol.PolicyRequest, bool, bool) {}

func (p *recordingPolicy) first(t *testing.T) protocol.PolicyRequest {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.seen) == 0 {
		t.Fatal("the policy was not asked")
	}
	return p.seen[0]
}

// policyAuth runs a real auth service with the policy before the exchange.
func policyAuth(t *testing.T, p *recordingPolicy) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := protocol.NewServer([]protocol.Passdb{disabledPassdb{}}, protocol.WithPolicy(p, protocol.PolicyMode{CheckBefore: true}))
	go srv.Serve(ctx, ln)                      //nolint:errcheck
	t.Cleanup(func() { cancel(); ln.Close() }) //nolint:errcheck
	return ln.Addr().String()
}

// A policy server's refusal message is what the mail client reads, in every
// protocol's own words.
func TestAPolicyRefusalReachesTheClient(t *testing.T) {
	const msg = "Too many failed logins, try again later"
	plain := base64.StdEncoding.EncodeToString([]byte("\x00alice\x00secret"))
	for _, tc := range []struct {
		proto Protocol
		lines []string
	}{
		{ProtocolIMAP, []string{"a1 LOGIN alice secret"}},
		{ProtocolPOP3, []string{"USER alice", "PASS secret"}},
		{ProtocolSubmission, []string{"EHLO client", "AUTH PLAIN " + plain}},
		{ProtocolManageSieve, []string{`AUTHENTICATE "PLAIN" "` + plain + `"`}},
	} {
		t.Run(string(tc.proto), func(t *testing.T) {
			p := &recordingPolicy{decision: protocol.PolicyDecision{Reject: true, Message: msg}}
			got := answerAfterGreeting(t, tc.proto, policyAuth(t, p), Options{}, tc.lines)
			if !strings.Contains(got, msg) {
				t.Errorf("answer %q does not carry %q", got, msg)
			}
		})
	}
}

// With imap_id_retain the client's ID pairs reach the policy as its device;
// without it nothing does.
func TestTheClientIDReachesThePolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		retain bool
		want   string
	}{
		{"kept", true, `"name" "Thunderbird" "version" "140.4.0"`},
		{"not kept", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &recordingPolicy{decision: protocol.PolicyDecision{Continue: true}}
			answerAfterGreeting(t, ProtocolIMAP, policyAuth(t, p), Options{IMAPIDRetain: tc.retain},
				[]string{`a0 ID ("name" "Thunderbird" "version" "140.4.0" "x-originating-ip" "192.0.2.9")`, "a1 LOGIN alice secret"})
			if got := p.first(t).DeviceID; got != tc.want {
				t.Errorf("device %q, want %q", got, tc.want)
			}
		})
	}
}

func TestKeptClientIDIsBounded(t *testing.T) {
	many := strings.Repeat(`"a" NIL `, 40)
	long := strings.Repeat("x", 300)
	for _, tc := range []struct {
		name, line, want string
	}{
		{"pairs quoted, NIL bare", `a ID ("name" "Mail" "os" NIL)`, `"name" "Mail" "os" NIL`},
		{"login keys left out", `a ID ("x-session-id" "s" "x-forward-foo" "f" "name" "Mail")`, `"name" "Mail"`},
		{"quotes escaped", `a ID ("name" "a\"b")`, `"name" "a\"b"`},
		{"a value past the length bound dropped", `a ID ("name" "Mail" "long" "` + long + `")`, `"name" "Mail"`},
		{"ID NIL keeps nothing", `a ID NIL`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := 0
			if got := keepClientID("", &n, tc.line); got != tc.want {
				t.Errorf("kept %q, want %q", got, tc.want)
			}
		})
	}
	n := 0
	kept := keepClientID("", &n, "a ID ("+many+")")
	if c := strings.Count(kept, "NIL"); c != maxKeptIDPairs {
		t.Errorf("kept %d pairs, want %d", c, maxKeptIDPairs)
	}
}

// An IMAPS login over TCP: the AUTH line carries TLS, the name the client
// asked for, its hello fingerprint, the ports and the real peer.
func TestTheAuthLineSaysWhatTheConnectionIs(t *testing.T) {
	serverTLS, clientTLS := cleartextTLS(t)
	got := make(chan string, 4)
	srv := New(testOpts(Options{Protocol: ProtocolIMAPS, AuthAddr: capturingAuth(t, got), BackendAddr: "127.0.0.1:1",
		LocalIP: "127.0.0.1", ExtTLS: serverTLS}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck
	go func() {
		if c, err := ln.Accept(); err == nil {
			srv.handleConn(c)
		}
	}()
	clientTLS.ServerName = "mail.example.org"
	c, err := tls.Dial("tcp", ln.Addr().String(), clientTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck
	rd := bufio.NewReader(c)
	rd.ReadString('\n') //nolint:errcheck
	fmt.Fprint(c, "a1 LOGIN alice secret\r\n")
	var line string
	select {
	case line = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("no AUTH reached the service")
	}
	_, clientPort, _ := net.SplitHostPort(c.LocalAddr().String())
	_, serverPort, _ := net.SplitHostPort(ln.Addr().String())
	for _, want := range []string{
		"\tsecured=tls", "\tlocal_name=mail.example.org", "\trport=" + clientPort, "\tlport=" + serverPort,
		"\treal_rip=127.0.0.1", "\treal_lip=127.0.0.1", "\treal_rport=" + clientPort, "\treal_lport=" + serverPort,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("AUTH %q lacks %q", line, want)
		}
	}
	if !regexp.MustCompile(`\tssl_ja3_hash=[0-9a-f]{32}(\t|\n|$)`).MatchString(line) {
		t.Errorf("AUTH %q has no JA3 hash", line)
	}
}

// The fingerprint is the reference's: GREASE values out of extensions and
// groups, kept in ciphers, the version the server's highest less one.
func TestJA3IsTheReferences(t *testing.T) {
	h := &tls.ClientHelloInfo{
		CipherSuites:    []uint16{0x0a0a, 4865, 4866},
		Extensions:      []uint16{0x1a1a, 0, 10, 11},
		SupportedCurves: []tls.CurveID{0x2a2a, 29, 23},
		SupportedPoints: []uint8{0},
	}
	sum := md5.Sum([]byte("771,2570-4865-4866,0-10-11,29-23,0")) //nolint:gosec // JA3
	if got := ja3Hash(h, tls.VersionTLS13); got != hex.EncodeToString(sum[:]) {
		t.Errorf("ja3 %s, want the hash of 771,2570-4865-4866,0-10-11,29-23,0", got)
	}
}

// answerAfterGreeting runs one login with extra options and returns the last
// reply.
func answerAfterGreeting(t *testing.T, proto Protocol, authAddr string, extra Options, lines []string) string {
	t.Helper()
	o := extra
	o.Protocol, o.AuthAddr, o.BackendAddr, o.LocalIP = proto, authAddr, "127.0.0.1:1", "127.0.0.1"
	srv := New(testOpts(o))
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() }) //nolint:errcheck
	go srv.handleConn(server)
	client.SetDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck
	rd := bufio.NewReader(client)
	greeting(t, proto, rd)
	var last string
	for _, line := range lines {
		fmt.Fprintf(client, "%s\r\n", line)
		last = finalLine(t, rd)
		if proto == ProtocolIMAP && strings.HasPrefix(last, "* ") {
			last = finalLine(t, rd)
		}
	}
	return last
}

// greeting reads the server's opening, which for ManageSieve is the
// capability list ending in OK.
func greeting(t *testing.T, proto Protocol, rd *bufio.Reader) {
	t.Helper()
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("greeting: %v", err)
		}
		if proto != ProtocolManageSieve || strings.HasPrefix(line, "OK") {
			return
		}
	}
}

// A relayed exchange refused with a reason answers with it, in every protocol.
func TestARelayedRefusalCarriesItsReason(t *testing.T) {
	const msg = "Too many failed logins, try again later"
	for _, p := range []Protocol{ProtocolIMAP, ProtocolPOP3, ProtocolSubmission, ProtocolManageSieve} {
		t.Run(string(p), func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close() //nolint:errcheck
			go func() {
				relayedFailure(server, p, "a1", &authclient.RefusedError{Reason: msg})
				server.Close() //nolint:errcheck
			}()
			line, _ := bufio.NewReader(client).ReadString('\n')
			if !strings.Contains(line, msg) {
				t.Errorf("answer %q does not carry %q", line, msg)
			}
		})
	}
}

// capturingAuth answers every AUTH with FAIL and hands the line over.
func capturingAuth(t *testing.T, got chan<- string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close() //nolint:errcheck
				fmt.Fprint(c, "VERSION\t1\t0\nMECH\tPLAIN\tplaintext\nSPID\t1\nDONE\n")
				rd := bufio.NewReader(c)
				for {
					line, err := rd.ReadString('\n')
					if err != nil {
						return
					}
					if f := strings.Split(strings.TrimRight(line, "\r\n"), "\t"); f[0] == "AUTH" {
						got <- line
						fmt.Fprintf(c, "FAIL\t%s\n", f[1])
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// A peer trusted to forward addresses is a proxy: its logins are not
// penalised for the clients behind it.
func TestATrustedPeerLogsInWithoutPenalty(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trusted string
		want    bool
	}{
		{"trusted", "127.0.0.0/8", true},
		{"not trusted", "192.0.2.0/24", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan string, 4)
			_, nets, _ := net.ParseCIDR(tc.trusted)
			srv := New(testOpts(Options{Protocol: ProtocolIMAP, AuthAddr: capturingAuth(t, got), BackendAddr: "127.0.0.1:1",
				LocalIP: "127.0.0.1", XClientNets: []*net.IPNet{nets}}))
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close() //nolint:errcheck
			go func() {
				if c, err := ln.Accept(); err == nil {
					srv.handleConn(c)
				}
			}()
			c, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close() //nolint:errcheck
			rd := bufio.NewReader(c)
			rd.ReadString('\n') //nolint:errcheck
			fmt.Fprint(c, "a1 LOGIN alice secret\r\n")
			select {
			case line := <-got:
				if has := strings.Contains(line, "\tno-penalty"); has != tc.want {
					t.Errorf("no-penalty %v in %q, want %v", has, line, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no AUTH reached the service")
			}
		})
	}
}

// Behind a PROXY header the client is the one the header names, and the real
// peer is the proxy itself.
func TestBehindAProxyTheRealPeerIsTheProxy(t *testing.T) {
	got := make(chan string, 4)
	_, loopback, _ := net.ParseCIDR("127.0.0.0/8")
	srv := New(testOpts(Options{Protocol: ProtocolIMAP, AuthAddr: capturingAuth(t, got), BackendAddr: "127.0.0.1:1",
		LocalIP: "127.0.0.1", HAProxy: true, HAProxyTimeout: 5 * time.Second, HAProxyNets: []*net.IPNet{loopback}}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln) //nolint:errcheck
	defer ln.Close() //nolint:errcheck
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck
	fmt.Fprint(c, "PROXY TCP4 192.0.2.10 198.51.100.20 51000 143\r\n")
	rd := bufio.NewReader(c)
	rd.ReadString('\n') //nolint:errcheck
	fmt.Fprint(c, "a1 LOGIN alice secret\r\n")
	var line string
	select {
	case line = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("no AUTH reached the service")
	}
	_, proxyPort, _ := net.SplitHostPort(c.LocalAddr().String())
	for _, want := range []string{
		"\trip=192.0.2.10", "\trport=51000", "\tlport=143",
		"\treal_rip=127.0.0.1", "\treal_rport=" + proxyPort,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("AUTH %q lacks %q", line, want)
		}
	}
}

// authLineOver runs one IMAP login over TCP, with lines sent first, and
// returns the AUTH line the service got.
func authLineOver(t *testing.T, opts Options, lines ...string) string {
	t.Helper()
	got := make(chan string, 4)
	opts.Protocol, opts.AuthAddr, opts.BackendAddr, opts.LocalIP = ProtocolIMAP, capturingAuth(t, got), "127.0.0.1:1", "127.0.0.1"
	srv := New(testOpts(opts))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)                 //nolint:errcheck
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() }) //nolint:errcheck
	rd := bufio.NewReader(c)
	for _, l := range lines {
		fmt.Fprint(c, l)
	}
	rd.ReadString('\n') //nolint:errcheck
	fmt.Fprint(c, "a1 LOGIN alice secret\r\n")
	select {
	case line := <-got:
		return line
	case <-time.After(5 * time.Second):
		t.Fatal("no AUTH reached the service")
	}
	return ""
}

// A trusted forwarder's client port is the client's port.
func TestAForwardedPortIsTheClientPort(t *testing.T) {
	_, loopback, _ := net.ParseCIDR("127.0.0.0/8")
	line := authLineOver(t, Options{XClient: true, XClientNets: []*net.IPNet{loopback}},
		`a0 ID ("x-originating-ip" "203.0.113.9" "x-originating-port" "54321")`+"\r\n")
	for _, want := range []string{"\trip=203.0.113.9", "\trport=54321", "\treal_rip=127.0.0.1"} {
		if !strings.Contains(line, want) {
			t.Errorf("AUTH %q lacks %q", line, want)
		}
	}
}

// Behind a PROXY header, trust is the proxy's own address, not the client's
// it names.
func TestNoPenaltyIsForTheRealPeer(t *testing.T) {
	_, loopback, _ := net.ParseCIDR("127.0.0.0/8")
	line := authLineOver(t, Options{HAProxy: true, HAProxyTimeout: 5 * time.Second,
		HAProxyNets: []*net.IPNet{loopback}, XClientNets: []*net.IPNet{loopback}},
		"PROXY TCP4 192.0.2.10 198.51.100.20 51000 143\r\n")
	if !strings.Contains(line, "\tno-penalty") {
		t.Errorf("AUTH %q: a trusted proxy's login is penalised", line)
	}
}
