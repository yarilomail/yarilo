package login

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/auth/authtest"
	authclient "github.com/yarilomail/yarilo/internal/auth/client"
	"github.com/yarilomail/yarilo/internal/auth/protocol"
)

func oauthRelay(t *testing.T, chain ...protocol.Passdb) relayContext {
	t.Helper()
	cl := authtest.RelayToChain(t, chain...)
	return relayContext{dial: func() (*authclient.Client, error) { return cl, nil }, sessionID: "s1"}
}

var validator = authtest.OAuthPassdb{User: "alice", Token: "good-token"}

// noTokens is a chain entry that validates no tokens and is not a validator.
type noTokens struct{}

func (noTokens) Authenticate(*protocol.Request) (protocol.Result, error) {
	return protocol.ResultNext, nil
}

func oauthMsg(mech, token string) string {
	if mech == "XOAUTH2" {
		return base64.StdEncoding.EncodeToString([]byte("user=alice\x01auth=Bearer " + token + "\x01\x01"))
	}
	return base64.StdEncoding.EncodeToString([]byte("n,a=alice,\x01auth=Bearer " + token + "\x01\x01"))
}

// protoCase drives one login protocol far enough to offer and use a mechanism.
type protoCase struct {
	name    string
	run     func(c net.Conn, rd *bufio.Reader, rc relayContext) (*preamble, error)
	caps    string // command that lists the mechanisms
	capsEnd string // the line that ends that listing
	auth    func(mech, b64 string) string
	cont    string // prefix of a challenge line
	refused string // prefix of the final refusal
}

var protoCases = []protoCase{
	{name: "IMAP", caps: "c CAPABILITY", capsEnd: "c OK",
		run: func(c net.Conn, rd *bufio.Reader, rc relayContext) (*preamble, error) {
			p, _, _, err := extractIMAPPreamble(c, rd, nil, Options{}, rc)
			return p, err
		},
		auth: func(m, b string) string { return "a1 AUTHENTICATE " + m + " " + b }, cont: "+ ", refused: "a1 NO"},
	{name: "POP3", caps: "CAPA", capsEnd: ".",
		run: func(c net.Conn, rd *bufio.Reader, rc relayContext) (*preamble, error) {
			p, _, _, err := extractPOP3Preamble(c, rd, nil, Options{}, rc)
			return p, err
		},
		auth: func(m, b string) string { return "AUTH " + m + " " + b }, cont: "+ ", refused: "-ERR"},
	{name: "SMTP", caps: "EHLO client", capsEnd: "250 ",
		run: func(c net.Conn, rd *bufio.Reader, rc relayContext) (*preamble, error) {
			p, _, _, err := extractSubmissionPreamble(c, rd, nil, Options{}, rc)
			return p, err
		},
		auth: func(m, b string) string { return "AUTH " + m + " " + b }, cont: "334 ", refused: "535"},
}

func startProto(t *testing.T, pc protoCase, rc relayContext) (net.Conn, *bufio.Reader, chan *preamble) {
	t.Helper()
	srv, cli := pipePair(t)
	done := make(chan *preamble, 1)
	go func() {
		p, _ := pc.run(srv, bufio.NewReader(srv), rc)
		done <- p
	}()
	cli.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	crd := bufio.NewReader(cli)
	crd.ReadString('\n') //nolint:errcheck // greeting
	return cli, crd, done
}

func readUntil(t *testing.T, rd *bufio.Reader, prefix string) []string {
	t.Helper()
	var lines []string
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("waiting for %q: %v (got %q)", prefix, err, lines)
		}
		line = strings.TrimRight(line, "\r\n")
		lines = append(lines, line)
		if strings.HasPrefix(line, prefix) {
			return lines
		}
	}
}

// Each proxy offers OAuth exactly when the service runs it.
func TestEveryProxyOffersOAuthOnlyWhenTheServiceRunsIt(t *testing.T) {
	for _, pc := range protoCases {
		for _, tc := range []struct {
			name string
			rc   relayContext
			want bool
		}{
			{"with a token validator", oauthRelay(t, validator), true},
			{"without one", oauthRelay(t, noTokens{}), false},
		} {
			t.Run(pc.name+" "+tc.name, func(t *testing.T) {
				cli, rd, _ := startProto(t, pc, tc.rc)
				fmt.Fprintf(cli, "%s\r\n", pc.caps)
				listing := strings.Join(readUntil(t, rd, pc.capsEnd), "\n")
				for _, m := range []string{"OAUTHBEARER", "XOAUTH2"} {
					if got := strings.Contains(listing, m); got != tc.want {
						t.Errorf("%s offered = %v, want %v:\n%s", m, got, tc.want, listing)
					}
				}
			})
		}
	}
}

// An offered OAuth mechanism logs in through the service.
func TestEveryProxyLogsInWithOAuth(t *testing.T) {
	for _, pc := range protoCases {
		for _, mech := range []string{"OAUTHBEARER", "XOAUTH2"} {
			t.Run(pc.name+" "+mech, func(t *testing.T) {
				cli, _, done := startProto(t, pc, oauthRelay(t, validator))
				fmt.Fprintf(cli, "%s\r\n", pc.auth(mech, oauthMsg(mech, "good-token")))
				go func() { _, _ = bufio.NewReader(cli).ReadString(0) }()
				select {
				case p := <-done:
					if p == nil || p.username != "alice" || p.authResult == nil {
						t.Fatalf("got %+v, want alice authenticated by the service", p)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("the login did not complete")
				}
			})
		}
	}
}

// A refused token is explained in a challenge, acknowledged, then refused.
func TestEveryProxyRelaysAnOAuthRefusal(t *testing.T) {
	for _, pc := range protoCases {
		for _, mech := range []string{"OAUTHBEARER", "XOAUTH2"} {
			t.Run(pc.name+" "+mech, func(t *testing.T) {
				cli, rd, done := startProto(t, pc, oauthRelay(t, validator))
				fmt.Fprintf(cli, "%s\r\n", pc.auth(mech, oauthMsg(mech, "bad-token")))
				lines := readUntil(t, rd, pc.cont)
				raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(lines[len(lines)-1], pc.cont))
				if err != nil || !strings.Contains(string(raw), `"status"`) {
					t.Fatalf("challenge %q, want the JSON refusal", lines[len(lines)-1])
				}
				fmt.Fprintf(cli, "%s\r\n", base64.StdEncoding.EncodeToString([]byte{1}))
				readUntil(t, rd, pc.refused)
				select {
				case p := <-done:
					t.Fatalf("the proxy returned %+v after a refusal", p)
				case <-time.After(100 * time.Millisecond):
				}
			})
		}
	}
}

// Submission offers SCRAM and carries it through the service, ending with the
// server signature and the client's empty acknowledgement.
func TestSubmissionRelaysSCRAM(t *testing.T) {
	smtp := protoCases[2]
	cli, rd, done := startProto(t, smtp, relayContext{dial: relayTo(t, relayService(t, []string{"SCRAM-SHA-256"}))})
	fmt.Fprintf(cli, "EHLO client\r\n")
	if listing := strings.Join(readUntil(t, rd, "250 "), "\n"); !strings.Contains(listing, "SCRAM-SHA-256") {
		t.Fatalf("EHLO does not offer SCRAM-SHA-256:\n%s", listing)
	}
	fmt.Fprintf(cli, "AUTH SCRAM-SHA-256 %s\r\n", base64.StdEncoding.EncodeToString([]byte("n,,n=alice,r=nonce")))
	readUntil(t, rd, "334 ")
	fmt.Fprintf(cli, "%s\r\n", base64.StdEncoding.EncodeToString([]byte("c=biws,r=nonce,p=proof")))
	final := readUntil(t, rd, "334 ")
	if v, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(final[len(final)-1], "334 ")); !strings.HasPrefix(string(v), "v=") {
		t.Fatalf("server-final %q, want the signature", final)
	}
	fmt.Fprintf(cli, "\r\n")
	select {
	case p := <-done:
		if p == nil || p.username != "alice" {
			t.Fatalf("got %+v, want alice", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the SCRAM login did not complete")
	}
}

// A mechanism nobody offered is still 504.
func TestSubmissionRefusesAnUnknownMechanism(t *testing.T) {
	cli, rd, _ := startProto(t, protoCases[2], oauthRelay(t, validator))
	fmt.Fprintf(cli, "AUTH CRAM-MD5\r\n")
	if lines := readUntil(t, rd, "5"); !strings.HasPrefix(lines[len(lines)-1], "504 ") {
		t.Fatalf("AUTH CRAM-MD5 = %q, want 504", lines)
	}
}

// A bearer token is a password: with cleartext off it is neither offered nor
// accepted before TLS.
func TestOAuthWaitsForTLSWithCleartextOff(t *testing.T) {
	srvTLS, _ := cleartextTLS(t)
	srv, cli := pipePair(t)
	go func() {
		_, _, _, _ = extractIMAPPreamble(remoteConn{srv}, bufio.NewReader(srv), srvTLS, Options{DisablePlainAuth: true}, oauthRelay(t, validator))
	}()
	cli.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	rd := bufio.NewReader(cli)
	greeting, _ := rd.ReadString('\n')
	if strings.Contains(greeting, "OAUTHBEARER") || strings.Contains(greeting, "XOAUTH2") {
		t.Errorf("greeting offers OAuth on an open line: %q", greeting)
	}
	fmt.Fprintf(cli, "a1 AUTHENTICATE XOAUTH2 %s\r\n", oauthMsg("XOAUTH2", "good-token"))
	if lines := readUntil(t, rd, "a1 "); !strings.HasPrefix(lines[len(lines)-1], "a1 NO [PRIVACYREQUIRED]") {
		t.Fatalf("AUTHENTICATE XOAUTH2 = %q, want NO [PRIVACYREQUIRED]", lines)
	}
}
