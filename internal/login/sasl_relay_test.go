package login

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	authclient "github.com/yarilomail/yarilo/internal/auth/client"
)

// relayService answers a SCRAM exchange the way yarilo-auth does, so the proxy
// is driven against the wire it will meet rather than against a stub of itself.
func relayService(t *testing.T, mechs []string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func() {
				defer c.Close() //nolint:errcheck
				rd := bufio.NewReader(c)
				fmt.Fprintf(c, "VERSION\t1\t0\n")
				for _, m := range mechs {
					fmt.Fprintf(c, "MECH\t%s\tactive\n", m)
				}
				fmt.Fprintf(c, "DONE\n")
				for {
					line, rerr := rd.ReadString('\n')
					if rerr != nil {
						return
					}
					f := strings.Split(strings.TrimRight(line, "\n"), "\t")
					switch f[0] {
					case "AUTH":
						fmt.Fprintf(c, "CONT\t%s\t%s\n", f[1],
							base64.StdEncoding.EncodeToString([]byte("r=nonce,s=c2FsdA==,i=4096")))
					case "CONT":
						fmt.Fprintf(c, "OK\t%s\tuser=alice\tresp=%s\n", f[1],
							base64.StdEncoding.EncodeToString([]byte("v=signature")))
					case "CANCEL":
						fmt.Fprintf(c, "OK\t%s\n", f[1])
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func relayTo(t *testing.T, addr string) relayDialer {
	t.Helper()
	cl, err := authclient.Dial(addr, nil)
	if err != nil {
		t.Fatalf("dial relay: %v", err)
	}
	t.Cleanup(func() { cl.Close() }) //nolint:errcheck
	return func() (*authclient.Client, error) { return cl, nil }
}

// The greeting offers what the service announced: the proxy runs no mechanism,
// so a name it cannot relay is a promise it cannot keep (#1733).
func TestTheGreetingOffersTheServiceMechanisms(t *testing.T) {
	dial := relayTo(t, relayService(t, []string{"PLAIN", "LOGIN", "SCRAM-SHA-256"}))
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() }) //nolint:errcheck

	go func() {
		_, _, _, _ = extractIMAPPreamble(server, bufio.NewReader(server), nil, Options{}, dial)
	}()

	greeting, err := bufio.NewReader(client).ReadString('\n')
	if err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	if !strings.Contains(greeting, "AUTH=SCRAM-SHA-256") {
		t.Errorf("greeting %q does not offer the mechanism the service runs", strings.TrimSpace(greeting))
	}
	// Not -PLUS: this connection is not TLS, so the proxy holds no binding.
	if strings.Contains(greeting, "SCRAM-SHA-256-PLUS") {
		t.Errorf("greeting %q offers -PLUS without a channel binding", strings.TrimSpace(greeting))
	}
}

// A mechanism the service does not announce is not advertised, even though the
// proxy could spell its name.
func TestTheProxyAdvertisesNothingTheServiceLacks(t *testing.T) {
	dial := relayTo(t, relayService(t, []string{"PLAIN", "LOGIN"}))
	_, server := net.Pipe()
	t.Cleanup(func() { server.Close() }) //nolint:errcheck

	if got := scramMechanisms(dial, server); len(got) != 0 {
		t.Errorf("the proxy offers %v against a service that announced no SCRAM", got)
	}
}

// The exchange runs end to end: the client's bytes reach the service and the
// verdict names the user, with no password anywhere in the proxy.
func TestTheProxyRelaysAScramExchange(t *testing.T) {
	dial := relayTo(t, relayService(t, []string{"SCRAM-SHA-256"}))
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() }) //nolint:errcheck

	type result struct {
		out *saslRelayOutcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		write, read := saslIO(server, bufio.NewReader(server))
		out, err := runRelayedSASL(dial, server, "SCRAM-SHA-256", "imap", "192.0.2.1", "s1",
			[]byte("n,,n=alice,r=nonce"), write, read)
		done <- result{out, err}
	}()

	rd := bufio.NewReader(client)
	line, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("read challenge: %v", err)
	}
	if !strings.HasPrefix(line, "+ ") {
		t.Fatalf("the proxy sent %q, want a challenge", line)
	}
	challenge, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(line[2:]))
	if derr != nil {
		t.Fatalf("challenge is not base64: %v", derr)
	}
	if !strings.HasPrefix(string(challenge), "r=") {
		t.Errorf("challenge %q is not the service's server-first message", challenge)
	}
	fmt.Fprintf(client, "%s\r\n", base64.StdEncoding.EncodeToString([]byte("c=biws,r=nonce,p=proof")))

	// RFC 5802 §5: the server signature reaches the client before the login is
	// announced, and the client acknowledges it with an empty response.
	final, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("read server-final: %v", err)
	}
	if !strings.HasPrefix(final, "+ ") {
		t.Fatalf("the proxy sent %q, want the server-final message", strings.TrimSpace(final))
	}
	signature, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(final[2:]))
	if derr != nil {
		t.Fatalf("server-final is not base64: %v", derr)
	}
	if !strings.HasPrefix(string(signature), "v=") {
		t.Errorf("the client was sent %q, which carries no server signature", signature)
	}
	fmt.Fprintf(client, "\r\n")

	got := <-done
	if got.err != nil {
		t.Fatalf("exchange: %v", got.err)
	}
	if got.out.username != "alice" {
		t.Errorf("verdict names %q, want alice", got.out.username)
	}
	if string(got.out.final) != "v=signature" {
		t.Errorf("server-final = %q, want the service's", got.out.final)
	}
}

// A client that cancels mid-exchange is a failure, not a login, and the proxy
// says so rather than carrying an empty identity onward.
func TestACancelledExchangeIsNotALogin(t *testing.T) {
	dial := relayTo(t, relayService(t, []string{"SCRAM-SHA-256"}))
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() }) //nolint:errcheck

	done := make(chan error, 1)
	go func() {
		write, read := saslIO(server, bufio.NewReader(server))
		_, err := runRelayedSASL(dial, server, "SCRAM-SHA-256", "imap", "192.0.2.1", "s1",
			[]byte("n,,n=alice,r=nonce"), write, read)
		done <- err
	}()

	rd := bufio.NewReader(client)
	if _, err := rd.ReadString('\n'); err != nil {
		t.Fatalf("read challenge: %v", err)
	}
	fmt.Fprintf(client, "*\r\n")

	if err := <-done; err == nil {
		t.Fatal("a cancelled exchange came back as a login")
	}
}

// The whole command loop: the proxy relays both rounds and the preamble comes
// back with an identity it never had a password for (#1733).
func TestTheCommandLoopCompletesAScramLogin(t *testing.T) {
	dial := relayTo(t, relayService(t, []string{"PLAIN", "LOGIN", "SCRAM-SHA-256"}))
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() }) //nolint:errcheck

	type result struct {
		pre *preamble
		err error
	}
	done := make(chan result, 1)
	go func() {
		pre, _, _, err := extractIMAPPreamble(server, bufio.NewReader(server), nil, Options{}, dial)
		done <- result{pre, err}
	}()

	rd := bufio.NewReader(client)
	if _, err := rd.ReadString('\n'); err != nil { // greeting
		t.Fatalf("greeting: %v", err)
	}
	fmt.Fprintf(client, "a1 AUTHENTICATE SCRAM-SHA-256 %s\r\n",
		base64.StdEncoding.EncodeToString([]byte("n,,n=alice,r=nonce")))

	line, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	if !strings.HasPrefix(line, "+ ") {
		t.Fatalf("the proxy answered %q, want a challenge", strings.TrimSpace(line))
	}
	fmt.Fprintf(client, "%s\r\n", base64.StdEncoding.EncodeToString([]byte("c=biws,r=nonce,p=proof")))

	// A client that cannot verify v= refuses a login the proxy just accepted,
	// so the signature must arrive before the identity is handed onward.
	final, ferr := rd.ReadString('\n')
	if ferr != nil {
		t.Fatalf("read server-final: %v", ferr)
	}
	signature, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(strings.TrimPrefix(final, "+ ")))
	if derr != nil || !strings.HasPrefix(string(signature), "v=") {
		t.Fatalf("the client was sent %q, which carries no server signature", strings.TrimSpace(final))
	}
	fmt.Fprintf(client, "\r\n")

	got := <-done
	if got.err != nil {
		t.Fatalf("preamble: %v", got.err)
	}
	if got.pre.username != "alice" {
		t.Errorf("preamble names %q, want alice", got.pre.username)
	}
	if got.pre.authResult == nil {
		t.Error("the preamble carries no verdict, so the proxy would authenticate again with a password it does not have")
	}
	if got.pre.password != "" {
		t.Error("the proxy kept a password out of a SCRAM exchange, which has none")
	}
}

// refusingRelay answers every exchange with a refusal, so a row can count what
// the proxy does with repeated failures.
func refusingRelay(t *testing.T) relayDialer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func() {
				defer c.Close() //nolint:errcheck
				rd := bufio.NewReader(c)
				fmt.Fprintf(c, "VERSION\t1\t0\nMECH\tSCRAM-SHA-256\tactive\nDONE\n")
				for {
					line, rerr := rd.ReadString('\n')
					if rerr != nil {
						return
					}
					f := strings.Split(strings.TrimRight(line, "\n"), "\t")
					if f[0] == "AUTH" || f[0] == "CONT" {
						fmt.Fprintf(c, "FAIL\t%s\n", f[1])
					}
				}
			}()
		}
	}()
	return relayTo(t, ln.Addr().String())
}

// A failed SCRAM exchange counts against the same per-connection ceiling as a
// failed LOGIN: without it, a client could try mechanisms forever (#1733).
func TestFailedScramExchangesHitTheAttemptLimit(t *testing.T) {
	dial := refusingRelay(t)
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() }) //nolint:errcheck

	done := make(chan error, 1)
	go func() {
		_, _, _, err := extractIMAPPreamble(server, bufio.NewReader(server), nil, Options{AuthMaxAttempts: 2}, dial)
		done <- err
	}()

	rd := bufio.NewReader(client)
	if _, err := rd.ReadString('\n'); err != nil { // greeting
		t.Fatalf("greeting: %v", err)
	}
	var lastReply string
	for i := 1; i <= 2; i++ {
		fmt.Fprintf(client, "a%d AUTHENTICATE SCRAM-SHA-256 %s\r\n", i,
			base64.StdEncoding.EncodeToString([]byte("n,,n=alice,r=nonce")))
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		lastReply = strings.TrimSpace(line)
	}
	if !strings.Contains(lastReply, "Too many failed authentications") {
		t.Errorf("after two refusals the proxy said %q, want the ceiling message", lastReply)
	}
	// Not a blocking read: without the ceiling the loop never returns, and a
	// row that hangs there reports a timeout instead of the defect.
	select {
	case err := <-done:
		if err == nil {
			t.Error("the loop returned a login after the ceiling was reached")
		}
	case <-time.After(2 * time.Second):
		t.Error("the loop kept going after the ceiling was reached")
	}
}

// One ceiling for both paths: a password failure and a refused SASL exchange
// count against the same configured number, with the same fallback.
func TestTheAttemptCeilingIsOneNumber(t *testing.T) {
	rows := []struct {
		name string
		opts Options
		want int
	}{
		{"configured", Options{AuthMaxAttempts: 7}, 7},
		{"unset falls back", Options{}, 3},
		{"negative falls back", Options{AuthMaxAttempts: -1}, 3},
	}
	for _, row := range rows {
		if got := authAttemptLimit(row.opts); got != row.want {
			t.Errorf("%s: limit = %d, want %d", row.name, got, row.want)
		}
	}
}
