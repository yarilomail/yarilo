package login

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	authclient "github.com/yarilomail/yarilo/internal/auth/client"
	"github.com/yarilomail/yarilo/internal/auth/protocol"
)

func TestTheProxyTimeoutIsTheUsersElseTheConfiguredOne(t *testing.T) {
	user := func(n int) *authclient.AuthResult {
		return &authclient.AuthResult{Userdb: &protocol.AuthResponse{ProxyTimeout: n}}
	}
	for _, tc := range []struct {
		name string
		opts time.Duration
		res  *authclient.AuthResult
		want time.Duration
	}{
		{"nothing set", 0, nil, 30 * time.Second},
		{"configured", 5 * time.Second, user(0), 5 * time.Second},
		{"the user's", 5 * time.Second, user(7), 7 * time.Second},
		{"a negative user value", 5 * time.Second, user(-1), 5 * time.Second},
		{"no userdb answer", 5 * time.Second, &authclient.AuthResult{}, 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{opts: Options{ProxyTimeout: tc.opts}}
			if got := s.proxyTimeout(tc.res); got != tc.want {
				t.Fatalf("proxyTimeout = %v, want %v", got, tc.want)
			}
		})
	}
}

// loginOnce runs one IMAP LOGIN through a proxy with the default transient
// retries and returns the tagged reply and how long it took.
func loginOnce(t *testing.T, authAddr, backend string, timeout time.Duration) (string, time.Duration) {
	t.Helper()
	wardenAddr, _ := startWardenWithHandle(t)
	s := &Server{
		opts: Options{
			Protocol:            ProtocolIMAP,
			AuthAddr:            authAddr,
			WardenAddr:          wardenAddr,
			BackendAddr:         backend,
			ProxyTimeout:        timeout,
			TransientReloginCap: 2,
		},
		sessions: make(map[string][]*liveSession),
	}
	t.Cleanup(func() {
		if s.wardenPool != nil {
			s.wardenPool.Close()
		}
	})
	srv, cli := pipePair(t)
	go s.handleConn(srv)
	crd := bufio.NewReader(cli)
	crd.ReadString('\n')                           //nolint:errcheck // greeting
	cli.Write([]byte("a1 LOGIN alice secret\r\n")) //nolint:errcheck
	start := time.Now()
	resp := readTagged(t, crd, "a1")
	return resp, time.Since(start)
}

// With the retries a silent backend still gets one window, not one per attempt.
func TestASilentBackendIsGivenOneProxyTimeout(t *testing.T) {
	for _, tc := range []struct {
		name, auth string
		timeout    time.Duration
		backend    []time.Duration // per connection: close after; past the list, silent
		within     time.Duration
		retries    float64
	}{
		{"configured", "", time.Second, nil, 1300 * time.Millisecond, 0},
		{"the user's", "\tproxy_timeout=1", time.Minute, nil, 1300 * time.Millisecond, 0},
		// Less than a pause left: no retry that would end past the window.
		{"fails with a pause left", "", time.Second, []time.Duration{900 * time.Millisecond}, 1300 * time.Millisecond, 0},
		// The second attempt gets what is left, not a window of its own.
		{"fails, then silent", "", time.Second, []time.Duration{600 * time.Millisecond}, 1300 * time.Millisecond, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retries := transientRetries.WithLabelValues("imap", stageBackendSession)
			before := testutil.ToFloat64(retries)
			resp, d := loginOnce(t, startOKAuthWith(t, tc.auth), startClosingBackend(t, tc.backend), tc.timeout)
			if d > tc.within {
				t.Fatalf("a1 took %v, want within %v", d, tc.within)
			}
			if n := testutil.ToFloat64(retries) - before; n != tc.retries {
				t.Fatalf("%v retries, want %v", n, tc.retries)
			}
			if !strings.Contains(resp, "UNAVAILABLE") {
				t.Fatalf("a1 = %q, want tagged NO [UNAVAILABLE]", resp)
			}
		})
	}
}

// startClosingBackend closes its i-th connection closeAfter[i] after accepting
// it, and holds any later one open without a word.
func startClosingBackend(t *testing.T, closeAfter []time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() {
		for n := 0; ; n++ {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() }) //nolint:errcheck
			if n < len(closeAfter) {
				time.AfterFunc(closeAfter[n], func() { c.Close() }) //nolint:errcheck
			}
		}
	}()
	return ln.Addr().String()
}

// startBackendFailingOnce drops its first connection after the preamble and
// greets on every later one.
func startBackendFailingOnce(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() {
		for n := 0; ; n++ {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() }) //nolint:errcheck
			go func(c net.Conn, first bool) {
				bufio.NewReader(c).ReadString('\n') //nolint:errcheck // preamble
				if first {
					c.Close() //nolint:errcheck
					return
				}
				c.Write([]byte("* OK [CAPABILITY IMAP4rev1] ready\r\n")) //nolint:errcheck
			}(c, n == 0)
		}
	}()
	return ln.Addr().String()
}

// One window over the attempts must not cost the retries themselves.
func TestABringUpThatFailsOnceStillLogsIn(t *testing.T) {
	resp, _ := loginOnce(t, startOKAuth(t), startBackendFailingOnce(t), 5*time.Second)
	if !strings.HasPrefix(resp, "a1 OK") {
		t.Fatalf("a1 = %q, want a1 OK after the retry", resp)
	}
}
