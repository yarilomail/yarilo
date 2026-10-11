package login

import (
	"bufio"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	authclient "github.com/yarilomail/yarilo/internal/auth/client"
	"github.com/yarilomail/yarilo/internal/auth/protocol"
)

func TestTheProxyTimeoutIsTheUsersElseTheConfiguredOne(t *testing.T) {
	user := func(v string) *authclient.AuthResult {
		return &authclient.AuthResult{Userdb: &protocol.AuthResponse{ProxyTimeout: v}}
	}
	for _, tc := range []struct {
		name string
		opts time.Duration
		res  *authclient.AuthResult
		want time.Duration
		bad  bool
	}{
		{name: "nothing set", res: nil, want: 30 * time.Second},
		{name: "configured", opts: 5 * time.Second, res: user(""), want: 5 * time.Second},
		{name: "a zero user value", opts: 5 * time.Second, res: user("0"), want: 5 * time.Second},
		{name: "the user's seconds", opts: 5 * time.Second, res: user("7"), want: 7 * time.Second},
		{name: "30s", opts: 5 * time.Second, res: user("30s"), want: 30 * time.Second},
		{name: "500ms", opts: 5 * time.Second, res: user("500ms"), want: 500 * time.Millisecond},
		{name: "2m", opts: 5 * time.Second, res: user("2m"), want: 2 * time.Minute},
		{name: "30 S", opts: 5 * time.Second, res: user("30 S"), want: 30 * time.Second},
		{name: "no userdb answer", opts: 5 * time.Second, res: &authclient.AuthResult{}, want: 5 * time.Second},
		{name: "-1", opts: 5 * time.Second, res: user("-1"), bad: true},
		{name: "abc", opts: 5 * time.Second, res: user("abc"), bad: true},
		{name: "past 2^32 ms", opts: 5 * time.Second, res: user("4294967296ms"), bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{opts: testOpts(Options{ProxyTimeout: tc.opts})}
			got, err := s.proxyTimeout(tc.res)
			if tc.bad {
				if err == nil {
					t.Fatalf("proxyTimeout = %v, want an error", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("proxyTimeout = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

// An unreadable proxy_timeout from either the passdb or the userdb half of the
// answer refuses the login before any backend is dialled.
func TestAnUnreadableProxyTimeoutRefusesTheLogin(t *testing.T) {
	for _, extra := range []string{
		"\tproxy_timeout=-1", "\tproxy_timeout=abc", "\tproxy_timeout=4294967296ms",
		"\tuserdb_proxy_timeout=-1", "\tuserdb_proxy_timeout=abc", "\tuserdb_proxy_timeout=4294967296ms",
	} {
		t.Run(strings.TrimPrefix(extra, "\t"), func(t *testing.T) {
			backend, dials := startCountingBackend(t)
			exhausted := transientExhausted.WithLabelValues("imap", stageBackendSession)
			before := testutil.ToFloat64(exhausted)
			resp, _ := loginOnce(t, startOKAuthWith(t, extra), backend, time.Second)
			if n := testutil.ToFloat64(exhausted) - before; n != 0 {
				t.Fatalf("a backend session was attempted (%v exhausted), want the login refused first", n)
			}
			if !strings.Contains(resp, "UNAVAILABLE") {
				t.Fatalf("a1 = %q, want tagged NO [UNAVAILABLE]", resp)
			}
			if n := dials.Load(); n != 0 {
				t.Fatalf("the backend was dialled %d times, want none", n)
			}
		})
	}
}

// startCountingBackend counts the connections it accepts and greets none.
func startCountingBackend(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			t.Cleanup(func() { c.Close() }) //nolint:errcheck
		}
	}()
	return ln.Addr().String(), &n
}

// loginOnce runs one IMAP LOGIN through a proxy with the default transient
// retries and returns the tagged reply and how long it took.
func loginOnce(t *testing.T, authAddr, backend string, timeout time.Duration) (string, time.Duration) {
	t.Helper()
	wardenAddr, _ := startWardenWithHandle(t)
	s := &Server{
		opts: testOpts(Options{
			Protocol:            ProtocolIMAP,
			AuthAddr:            authAddr,
			WardenAddr:          wardenAddr,
			WardenConns:         4,
			BackendAddr:         backend,
			ProxyTimeout:        timeout,
			TransientRetries:    3,
			TransientReloginCap: 2,
		}),
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
		{"the user's in units", "\tuserdb_proxy_timeout=500ms", time.Minute, nil, 800 * time.Millisecond, 0},
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
