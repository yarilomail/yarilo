package lmtplogin

import (
	"net"
	"testing"
	"time"
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
		"alice@example.com": {Username: "alice@example.com", ProxyTimeout: 1},
		"bob@example.com":   {Username: "bob@example.com"},
	})
	backend := silentBackend(t)
	slow := startLMTPLogin(t, Options{Hostname: "test.local", BackendAddr: backend,
		AuthMasterAddr: authAddr, ProxyTimeout: time.Minute})
	deliverWithin(t, slow, "alice@example.com", 5*time.Second)

	fast := startLMTPLogin(t, Options{Hostname: "test.local", BackendAddr: backend,
		AuthMasterAddr: authAddr, ProxyTimeout: 300 * time.Millisecond})
	deliverWithin(t, fast, "bob@example.com", 900*time.Millisecond)
}

// A user without proxy_timeout, or with a negative one (the userdb lets it
// through), keeps the global value instead of having the connection closed.
func TestWithoutAPositiveProxyTimeoutTheGlobalOneApplies(t *testing.T) {
	authAddr := startTestAuthWithUserdb(t, fakeUserdb{
		"none@example.com": {Username: "none@example.com"},
		"neg@example.com":  {Username: "neg@example.com", ProxyTimeout: -1},
	})
	_, backendAddr := newStubBackend(t)
	proxyAddr := startLMTPLogin(t, Options{Hostname: "test.local", BackendAddr: backendAddr,
		AuthMasterAddr: authAddr})
	for _, rcpt := range []string{"none@example.com", "neg@example.com"} {
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
