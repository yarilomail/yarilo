package pop3

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // SCRAM-SHA-1 is a mechanism under test, not a choice
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"hash"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"golang.org/x/crypto/pbkdf2"

	"github.com/yarilomail/yarilo/v2/internal/auth/authtest"
	"github.com/yarilomail/yarilo/v2/internal/auth/protocol"
)

// scramUsers answers SCRAM verifiers for one user and refuses plain auth, so
// a login that succeeds can only have come through SCRAM.
type scramUsers struct {
	user     string
	s256, s1 *sasl.ScramCredentials
}

func (a *scramUsers) Authenticate(_, _, _, _ string) (*protocol.AuthResponse, error) {
	return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
}
func (a *scramUsers) LookupSCRAMSha256(u string) (*sasl.ScramCredentials, error) {
	if u == a.user {
		return a.s256, nil
	}
	return nil, nil
}
func (a *scramUsers) LookupSCRAMSha1(u string) (*sasl.ScramCredentials, error) {
	if u == a.user {
		return a.s1, nil
	}
	return nil, nil
}

// tcpPOP3Session is newPOP3Session over a buffered loopback connection: an
// extra reply the server writes then lands in the stream, where the next
// command's assertion sees it, instead of blocking an unbuffered pipe.
func tcpPOP3Session(t *testing.T, opts Options) (net.Conn, *bufio.Reader) {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	srv := New(opts)
	go func() {
		s, err := ln.Accept()
		if err != nil {
			return
		}
		srv.newSession(s).serve()
	}()
	c, err := new(net.Dialer).DialContext(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	t.Cleanup(func() { _ = c.Close() })
	r := bufio.NewReader(c)
	if g := readline(t, r); !strings.HasPrefix(g, "+OK") {
		t.Fatalf("greeting %q", g)
	}
	return c, r
}

// scramLogin runs AUTH <mech> as a client would, and returns the server's
// final line.
func scramLogin(t *testing.T, c net.Conn, r *bufio.Reader, mech string, newHash func() hash.Hash, size int, user, pass string) string {
	t.Helper()
	nonce := "fyko+d2lbbFgONRv9qkxdawL"
	first := "n=" + user + ",r=" + nonce
	send(t, c, "AUTH "+mech+" "+base64.StdEncoding.EncodeToString([]byte("n,,"+first)))
	line := readline(t, r)
	if !strings.HasPrefix(line, "+ ") {
		return line
	}
	sf, _ := base64.StdEncoding.DecodeString(line[2:])
	var combined, saltB64 string
	var iter int
	for _, attr := range strings.Split(string(sf), ",") {
		switch {
		case strings.HasPrefix(attr, "r="):
			combined = attr[2:]
		case strings.HasPrefix(attr, "s="):
			saltB64 = attr[2:]
		case strings.HasPrefix(attr, "i="):
			_, _ = fmt.Sscanf(attr[2:], "%d", &iter)
		}
	}
	salt, _ := base64.StdEncoding.DecodeString(saltB64)
	without := "c=" + base64.StdEncoding.EncodeToString([]byte("n,,")) + ",r=" + combined
	authMsg := first + "," + string(sf) + "," + without
	salted := pbkdf2.Key([]byte(pass), salt, iter, size, newHash)
	mac := func(key, data []byte) []byte { m := hmac.New(newHash, key); m.Write(data); return m.Sum(nil) }
	clientKey := mac(salted, []byte("Client Key"))
	h := newHash()
	h.Write(clientKey)
	sig := mac(h.Sum(nil), []byte(authMsg))
	proof := make([]byte, len(clientKey))
	for i := range proof {
		proof[i] = clientKey[i] ^ sig[i]
	}
	send(t, c, base64.StdEncoding.EncodeToString([]byte(without+",p="+base64.StdEncoding.EncodeToString(proof))))
	return readline(t, r)
}

// SCRAM in both families logs the user in with the right password and only
// with it; a family the user has no verifier for fails, as does a cancelled
// exchange.
func TestSessionSCRAM(t *testing.T) {
	s256, err := sasl.GenerateScramSha256Credentials("pw", 4096)
	if err != nil {
		t.Fatal(err)
	}
	s1, err := sasl.GenerateScramSha1Credentials("pw", 4096)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, mech string
		newHash    func() hash.Hash
		size       int
		pass       string
		users      *scramUsers
		ok         bool
	}{
		{"sha256", "SCRAM-SHA-256", sha256.New, sha256.Size, "pw", &scramUsers{user: "alice", s256: s256, s1: s1}, true},
		{"sha1", "SCRAM-SHA-1", sha1.New, sha1.Size, "pw", &scramUsers{user: "alice", s256: s256, s1: s1}, true},
		{"sha256 wrong password", "SCRAM-SHA-256", sha256.New, sha256.Size, "nope", &scramUsers{user: "alice", s256: s256, s1: s1}, false},
		{"sha1 without a verifier", "SCRAM-SHA-1", sha1.New, sha1.Size, "pw", &scramUsers{user: "alice", s256: s256}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := newTestOpts(t, nil, &mockMailbox{}, &mockIndex{})
			opts.AuthRelay = authtest.RelayTo(t, tc.users)
			c, r := tcpPOP3Session(t, opts)
			final := scramLogin(t, c, r, tc.mech, tc.newHash, tc.size, "alice", tc.pass)
			if got := strings.HasPrefix(final, "+OK"); got != tc.ok {
				t.Fatalf("AUTH %s: %q, want ok=%v", tc.mech, final, tc.ok)
			}
			// STAT's own answer, not a second reply to AUTH left in the stream.
			send(t, c, "STAT")
			stat := readline(t, r)
			if want := "+OK 0 0"; tc.ok && stat != want {
				t.Errorf("STAT after AUTH %s: %q, want %q", tc.mech, stat, want)
			} else if !tc.ok && !strings.HasPrefix(stat, "-ERR") {
				t.Errorf("STAT after a refused AUTH %s: %q, want -ERR", tc.mech, stat)
			}
		})
	}

	opts := newTestOpts(t, nil, &mockMailbox{}, &mockIndex{})
	opts.AuthRelay = authtest.RelayTo(t, &scramUsers{user: "alice", s256: s256})
	c, r := tcpPOP3Session(t, opts)
	send(t, c, "AUTH SCRAM-SHA-256")
	if line := readline(t, r); line != "+ " {
		t.Fatalf("AUTH without an initial response: %q, want an empty challenge", line)
	}
	send(t, c, "*")
	if line := readline(t, r); !strings.HasPrefix(line, "-ERR") {
		t.Errorf("a cancelled exchange: %q, want -ERR", line)
	}
	// The next command gets its own answer: one -ERR for the cancel, not two.
	send(t, c, "CAPA")
	if line := readline(t, r); !strings.HasPrefix(line, "+OK") {
		t.Errorf("CAPA after a cancelled exchange: %q, want its +OK", line)
	}
	readUntilDot(t, r)
	send(t, c, "AUTH SCRAM-SHA-256-PLUS")
	if line := readline(t, r); !strings.HasPrefix(line, "-ERR") {
		t.Errorf("-PLUS without TLS: %q, want -ERR", line)
	}
}

// XOAUTH2 logs in with the right token, refuses a wrong one, and is not
// offered when OAuth2 is off.
func TestSessionXOAuth2(t *testing.T) {
	payload := func(user, token string) string {
		return base64.StdEncoding.EncodeToString([]byte("user=" + user + "\x01auth=Bearer " + token + "\x01\x01"))
	}
	tests := []struct {
		name    string
		enabled bool
		line    string
		ok      bool
	}{
		{"valid", true, "AUTH XOAUTH2 " + payload("alice@test.com", "valid-token"), true},
		{"wrong token", true, "AUTH XOAUTH2 " + payload("alice@test.com", "WRONG"), false},
		{"bad base64", true, "AUTH XOAUTH2 !!!", false},
		{"disabled", false, "AUTH XOAUTH2 " + payload("alice@test.com", "valid-token"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := newTestOpts(t, nil, &mockMailbox{}, &mockIndex{})
			opts.AuthRelay = authtest.RelayTo(t, &oauthBearerAuth{user: "alice@test.com", token: "valid-token"})
			opts.OAuth2Enabled = tc.enabled
			c, r := newPOP3Session(t, opts)
			send(t, c, tc.line)
			if got := strings.HasPrefix(readline(t, r), "+OK"); got != tc.ok {
				t.Errorf("%s: ok=%v, want %v", tc.name, got, tc.ok)
			}
		})
	}
}
