package lmtplogin

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
)

// silentBackend accepts and never answers.
func silentBackend(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { conn.Close() })
		}
	}()
	return ln.Addr().String()
}

// deliverWithin delivers one message to rcpt and fails unless the backend
// timeout ends it, as a 4xx, within limit.
func deliverWithin(t *testing.T, proxyAddr, rcpt string, limit time.Duration) {
	t.Helper()
	mta := dialMTA(t, proxyAddr)
	mta.lmtpHandshake(t)
	mta.mailFrom(t, "sender@example.com")
	mta.rcpt(t, rcpt)
	start := time.Now()
	mta.data(t, "Subject: Test\r\n\r\nHello")
	codes := mta.readDataStatuses(t, 1)
	if elapsed := time.Since(start); elapsed > limit {
		t.Fatalf("delivery to %s failed after %v, want within %v", rcpt, elapsed, limit)
	}
	if codes[0]/100 != 4 {
		t.Fatalf("data status = %d, want a 4xx temporary failure", codes[0])
	}
}

// A backend that never answers: the delivery must fail at ProxyTimeout, not
// at the default.
func TestProxyTimeoutReachesTheBackendCall(t *testing.T) {
	proxyAddr := startLMTPLogin(t, Options{
		Hostname:       "test.local",
		BackendAddr:    silentBackend(t),
		AuthMasterAddr: startTestAuth(t),
		ProxyTimeout:   300 * time.Millisecond,
	})
	deliverWithin(t, proxyAddr, "alice@example.com", 5*time.Second)
}

// The global cap outlasts the MTA's 10s deadline, so only the user's
// proxy_timeout ends the delivery in time; without one the global value holds.
func TestAUsersProxyTimeoutOverridesTheGlobalOne(t *testing.T) {
	authAddr := startTestAuthWithUserdb(t, fakeUserdb{
		"alice@example.com": {Username: "alice@example.com", ProxyTimeout: "1"},
		"carol@example.com": {Username: "carol@example.com", ProxyTimeout: "500ms"},
		"bob@example.com":   {Username: "bob@example.com"},
	})
	backend := silentBackend(t)
	slow := startLMTPLogin(t, Options{Hostname: "test.local", BackendAddr: backend,
		AuthMasterAddr: authAddr, ProxyTimeout: time.Minute})
	deliverWithin(t, slow, "alice@example.com", 5*time.Second)
	deliverWithin(t, slow, "carol@example.com", 5*time.Second)

	fast := startLMTPLogin(t, Options{Hostname: "test.local", BackendAddr: backend,
		AuthMasterAddr: authAddr, ProxyTimeout: 300 * time.Millisecond})
	deliverWithin(t, fast, "bob@example.com", 900*time.Millisecond)
}

// A user without proxy_timeout, or with 0, keeps the global value instead of
// having the connection closed.
func TestWithoutAProxyTimeoutTheGlobalOneApplies(t *testing.T) {
	authAddr := startTestAuthWithUserdb(t, fakeUserdb{
		"none@example.com": {Username: "none@example.com"},
		"zero@example.com": {Username: "zero@example.com", ProxyTimeout: "0"},
	})
	_, backendAddr := newStubBackend(t)
	proxyAddr := startLMTPLogin(t, Options{Hostname: "test.local", BackendAddr: backendAddr,
		AuthMasterAddr: authAddr})
	for _, rcpt := range []string{"none@example.com", "zero@example.com"} {
		mta := dialMTA(t, proxyAddr)
		mta.lmtpHandshake(t)
		mta.mailFrom(t, "sender@example.com")
		mta.rcpt(t, rcpt)
		mta.data(t, "Subject: Test\r\n\r\nHello")
		if codes := mta.readDataStatuses(t, 1); codes[0] != 250 {
			t.Errorf("%s: data status = %d, want 250", rcpt, codes[0])
		}
	}
}

// rcptReply sends RCPT TO and returns the final reply line.
func (c *mtaConn) rcptReply(t *testing.T, rcpt string) string {
	t.Helper()
	fmt.Fprintf(c.conn, "RCPT TO:<%s>\r\n", rcpt)
	for {
		line, err := c.rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read RCPT response: %v", err)
		}
		if line = strings.TrimRight(line, "\r\n"); len(line) >= 4 && line[3] == ' ' {
			return line
		}
	}
}

// A proxy_timeout this proxy cannot read fails the recipient for good; a
// lookup that fails is temporary.
func TestTheUserLookupDecidesTheRecipientReply(t *testing.T) {
	authAddr := startTestAuthWithUserdb(t, failingUserdb{fakeUserdb{
		"neg@example.com":  {Username: "neg@example.com", ProxyTimeout: "-1"},
		"abc@example.com":  {Username: "abc@example.com", ProxyTimeout: "abc"},
		"huge@example.com": {Username: "huge@example.com", ProxyTimeout: "4294967296ms"},
	}})
	proxyAddr := startLMTPLogin(t, Options{Hostname: "test.local", BackendAddr: silentBackend(t),
		AuthMasterAddr: authAddr})
	for _, tc := range []struct{ rcpt, want string }{
		{"neg@example.com", "550 5.3.5 Internal user lookup failure"},
		{"abc@example.com", "550 5.3.5 Internal user lookup failure"},
		{"huge@example.com", "550 5.3.5 Internal user lookup failure"},
		{"down@example.com", "451 4.3.0 Temporary user lookup failure"},
	} {
		mta := dialMTA(t, proxyAddr)
		mta.lmtpHandshake(t)
		mta.mailFrom(t, "sender@example.com")
		if got := mta.rcptReply(t, tc.rcpt); !strings.HasPrefix(got, tc.want) {
			t.Errorf("%s: RCPT = %q, want %q", tc.rcpt, got, tc.want)
		}
	}
}

// failingUserdb answers like fakeUserdb and fails the lookup for down@.
type failingUserdb struct{ fakeUserdb }

func (f failingUserdb) Lookup(username string) (*protocol.UserInfo, error) {
	if username == "down@example.com" {
		return nil, errors.New("userdb unavailable")
	}
	return f.fakeUserdb.Lookup(username)
}
