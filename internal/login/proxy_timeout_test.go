package login

import (
	"bufio"
	"strings"
	"testing"
	"time"

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

// The configured cap outlasts the test, so only the proxy_timeout that auth
// sends can end the bring-up against a backend that never greets.
func TestAUsersProxyTimeoutBoundsTheBringUp(t *testing.T) {
	wardenAddr, _ := startWardenWithHandle(t)
	s := &Server{
		opts: Options{
			Protocol:            ProtocolIMAP,
			AuthAddr:            startOKAuthWith(t, "\tproxy_timeout=1"),
			WardenAddr:          wardenAddr,
			BackendAddr:         startSilentBackend(t),
			ProxyTimeout:        time.Minute,
			TransientRetries:    -1,
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
	crd.ReadString('\n') //nolint:errcheck // greeting

	cli.Write([]byte("a1 LOGIN alice secret\r\n")) //nolint:errcheck
	start := time.Now()
	resp := readTagged(t, crd, "a1")
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("a1 took %v, want about the user's 1s proxy_timeout", d)
	}
	if !strings.Contains(resp, "UNAVAILABLE") {
		t.Fatalf("a1 = %q, want tagged NO [UNAVAILABLE]", resp)
	}
}
