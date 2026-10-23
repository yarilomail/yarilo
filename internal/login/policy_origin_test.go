package login

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
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

func TestTheAuthLineSaysWhatTheConnectionIs(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()                                                 //nolint:errcheck
	defer c2.Close()                                                 //nolint:errcheck
	tlsConn := tls.Client(c1, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec
	for _, tc := range []struct {
		name      string
		conn      net.Conn
		clientID  string
		noPenalty bool
		want      int
	}{
		{"plain, nothing known", c1, "", false, 0},
		{"TLS", tlsConn, "", false, 1},
		{"TLS, an ID and a trusted peer", tlsConn, `"name" "x"`, true, 3},
	} {
		if got := len(authOrigin(tc.conn, tc.clientID, tc.noPenalty)); got != tc.want {
			t.Errorf("%s: %d options, want %d", tc.name, got, tc.want)
		}
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
