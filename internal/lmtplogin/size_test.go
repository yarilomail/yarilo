package lmtplogin

import (
	"fmt"
	"strings"
	"testing"
)

// lineOf reads one whole reply and returns its last line.
func (c *mtaConn) lineOf(t *testing.T) string {
	t.Helper()
	var all []string
	for {
		line, err := c.rd.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		all = append(all, line)
		if len(line) < 4 || line[3] == ' ' {
			return strings.Join(all, "\n")
		}
	}
}

func (c *mtaConn) send(t *testing.T, format string, a ...any) string {
	t.Helper()
	fmt.Fprintf(c.conn, format+"\r\n", a...)
	return c.lineOf(t)
}

// The proxy offers quota_mail_size as SIZE and enforces it while reading, so
// an oversized body is refused before it is held or relayed.
func TestTheProxyOffersAndEnforcesTheMailSize(t *testing.T) {
	const limit = 200
	authAddr := startTestAuth(t)
	_, backendAddr := newStubBackend(t)
	proxyAddr := startLMTPLogin(t, Options{
		Hostname: "test.local", BackendAddr: backendAddr, AuthMasterAddr: authAddr,
		WardenAddr: startTestWarden(t), ConcurrencyLimit: 5, MaxMessageBytes: limit,
	})

	t.Run("LHLO offers SIZE", func(t *testing.T) {
		mta := dialMTA(t, proxyAddr)
		mta.readCode(t, 220)
		if r := mta.send(t, "LHLO mta.test"); !strings.Contains(r, fmt.Sprintf("SIZE %d", limit)) {
			t.Errorf("LHLO does not offer SIZE %d:\n%s", limit, r)
		}
	})
	t.Run("MAIL FROM SIZE over the limit is refused", func(t *testing.T) {
		mta := dialMTA(t, proxyAddr)
		mta.lmtpHandshake(t)
		if r := mta.send(t, "MAIL FROM:<a@external.test> SIZE=%d", limit+1); !strings.HasPrefix(r, "552") {
			t.Errorf("MAIL FROM with SIZE=%d answered %q, want 552", limit+1, r)
		}
	})
	t.Run("a body over the limit is refused and the session goes on", func(t *testing.T) {
		mta := dialMTA(t, proxyAddr)
		mta.lmtpHandshake(t)
		mta.mailFrom(t, "a@external.test")
		mta.rcpt(t, "alice@example.com")
		mta.data(t, "Subject: big\r\n\r\n"+strings.Repeat("x", limit*2))
		if r := mta.lineOf(t); !strings.HasPrefix(r, "552 5.3.4") {
			t.Errorf("an oversized body answered %q, want 552 5.3.4 while reading", r)
		}
		mta.mailFrom(t, "a@external.test")
		mta.rcpt(t, "alice@example.com")
		mta.data(t, "Subject: small\r\n\r\nhi")
		if r := mta.lineOf(t); !strings.HasPrefix(r, "250") {
			t.Errorf("a small message after the refusal answered %q", r)
		}
	})
}

func TestTheProxyHoldsTheRecipientLimit(t *testing.T) {
	_, backendAddr := newStubBackend(t)
	proxyAddr := startLMTPLogin(t, Options{
		Hostname: "test.local", BackendAddr: backendAddr, AuthMasterAddr: startTestAuth(t), MaxRecipients: 2,
	})
	mta := dialMTA(t, proxyAddr)
	mta.readCode(t, 220)
	fmt.Fprintf(mta.conn, "LHLO smoketest\r\n")
	if caps := mta.lineOf(t); !strings.Contains(caps, "LIMITS RCPTMAX=2") {
		t.Errorf("LHLO does not advertise LIMITS RCPTMAX=2:\n%s", caps)
	}
	mta.mailFrom(t, "sender@example.com")
	mta.rcpt(t, "a@example.com")
	mta.rcpt(t, "b@example.com")
	if code := mta.tryRcpt(t, "c@example.com"); code != 452 {
		t.Fatalf("third RCPT = %d, want 452", code)
	}
}
