package jmaplogin

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// upgradeBackend switches every request to an echo of the raw connection, the
// shape of a WebSocket upgrade without its framing.
func upgradeBackend(t *testing.T) string {
	t.Helper()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "upgrade only", http.StatusBadRequest)
			return
		}
		c, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer c.Close() //nolint:errcheck
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = brw.Flush()
		_, _ = io.Copy(c, brw)
	}))
	t.Cleanup(b.Close)
	return b.Listener.Addr().String()
}

// testTLS returns a server config and a client config that trusts it.
func testTLS(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	ts := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(ts.Close)
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	return ts.TLS.Clone(), &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
}

// openUpgrade upgrades a connection through the proxy and returns it raw; a
// non-nil client config dials TLS.
func openUpgrade(t *testing.T, base string, client *tls.Config) net.Conn {
	t.Helper()
	addr := strings.TrimPrefix(base, "http://")
	var c net.Conn
	var err error
	if client != nil {
		c, err = tls.Dial("tcp", addr, client)
	} else {
		c, err = net.Dial("tcp", addr)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() }) //nolint:errcheck
	req := "GET /jmap/ws/ HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n" +
		"Authorization: " + basic("u1", "pw") + "\r\n\r\n"
	if _, err := c.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d, want 101", resp.StatusCode)
	}
	_ = c.SetReadDeadline(time.Time{})
	return c
}

func echoes(c net.Conn) bool {
	if _, err := c.Write([]byte("ping")); err != nil {
		return false
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	defer c.SetReadDeadline(time.Time{}) //nolint:errcheck
	buf := make([]byte, 4)
	_, err := io.ReadFull(c, buf)
	return err == nil && string(buf) == "ping"
}

// closedWithin reports whether the peer closes c within the wait.
func closedWithin(c net.Conn, within time.Duration) bool {
	_ = c.SetReadDeadline(time.Now().Add(within))
	_, err := c.Read(make([]byte, 1))
	var ne net.Error
	if err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		return false
	}
	return true
}

func eventually(cond func() bool) bool {
	for i := 0; i < 100; i++ {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// An upgraded connection stays accounted while it is open and is released
// when it closes, in clear and under TLS.
func TestAnUpgradedConnectionStaysAccounted(t *testing.T) {
	serverTLS, clientTLS := testTLS(t)
	for _, tc := range []struct {
		name           string
		server, client *tls.Config
	}{
		{"clear", nil, nil},
		{"tls", serverTLS, clientTLS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &fakeWarden{}
			_, base := startServer(t, Options{Router: StaticRouter{Addr: upgradeBackend(t)}, Warden: w, ExtTLS: tc.server})
			c := openUpgrade(t, base, tc.client)
			if !echoes(c) {
				t.Fatal("the upgraded connection does not carry bytes")
			}
			if n := w.countOp("connect"); n != 1 {
				t.Fatalf("%d connects, want 1", n)
			}
			if n := w.countOp("disconnect"); n != 0 {
				t.Errorf("%d disconnects while the connection is open, want 0", n)
			}
			_ = c.Close()
			if !eventually(func() bool { return w.countOp("disconnect") == 1 }) {
				t.Errorf("%d disconnects after the close, want 1", w.countOp("disconnect"))
			}
		})
	}
}

// A kick, by session or by user, closes an upgraded connection.
func TestAKickEndsAnUpgradedConnection(t *testing.T) {
	for _, tc := range []struct {
		name string
		kick func(s *Server, id string) bool
	}{
		{"by session", func(s *Server, id string) bool { return s.kickSession(id) }},
		{"by user", func(s *Server, _ string) bool { return s.kickUser("u1", "admin") == 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &fakeWarden{}
			s, base := startServer(t, Options{Router: StaticRouter{Addr: upgradeBackend(t)}, Warden: w})
			c := openUpgrade(t, base, nil)
			if !echoes(c) {
				t.Fatal("the upgraded connection does not carry bytes")
			}
			calls := w.snapshot()
			if len(calls) == 0 {
				t.Fatal("the connection was never accounted")
			}
			if !tc.kick(s, calls[0].id) {
				t.Fatal("the kick found no connection")
			}
			if !closedWithin(c, time.Second) {
				t.Error("the upgraded connection outlived the kick")
			}
			if !eventually(func() bool { return w.countOp("disconnect") == 1 }) {
				t.Errorf("%d disconnects after the kick, want 1", w.countOp("disconnect"))
			}
		})
	}
}
