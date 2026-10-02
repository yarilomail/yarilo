package lmtplogin

import (
	"net"
	"testing"
	"time"
)

// A backend that accepts and never answers: the delivery must fail at
// ProxyTimeout, not at the default.
func TestProxyTimeoutReachesTheBackendCall(t *testing.T) {
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

	proxyAddr := startLMTPLogin(t, Options{
		Hostname:       "test.local",
		BackendAddr:    ln.Addr().String(),
		AuthMasterAddr: startTestAuth(t),
		ProxyTimeout:   300 * time.Millisecond,
	})

	mta := dialMTA(t, proxyAddr)
	mta.lmtpHandshake(t)
	mta.mailFrom(t, "sender@example.com")
	mta.rcpt(t, "alice@example.com")
	start := time.Now()
	mta.data(t, "Subject: Test\r\n\r\nHello")
	codes := mta.readDataStatuses(t, 1)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("delivery failed after %v, want about the 300ms ProxyTimeout", elapsed)
	}
	if codes[0]/100 != 4 {
		t.Fatalf("data status = %d, want a 4xx temporary failure", codes[0])
	}
}

func TestAZeroProxyTimeoutTakesTheDefault(t *testing.T) {
	if got := New(Options{}).opts.ProxyTimeout; got != 125*time.Second {
		t.Fatalf("ProxyTimeout = %v, want 125s", got)
	}
}
