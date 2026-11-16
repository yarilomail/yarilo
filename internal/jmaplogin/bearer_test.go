package jmaplogin

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authclient "github.com/yarilomail/yarilo/v2/internal/auth/client"
	"github.com/yarilomail/yarilo/v2/internal/auth/oauth2"
	"github.com/yarilomail/yarilo/v2/internal/auth/protocol"
)

// tokenValidator names alice for good-token and bob for barred-token.
type tokenValidator struct{}

func (tokenValidator) Validate(_ context.Context, token string) (*oauth2.Claims, error) {
	switch token {
	case "good-token":
		return &oauth2.Claims{Username: "alice"}, nil
	case "barred-token":
		return &oauth2.Claims{Username: "bob"}, nil
	}
	return nil, oauth2.ErrTokenInvalid
}

// barred marks bob nologin after the OAuth2 passdb accepted him.
type barred struct{ *oauth2.Passdb }

func (b barred) Authenticate(req *protocol.Request) (protocol.Result, error) {
	r, err := b.Passdb.Authenticate(req)
	if u, _ := req.Fields.Get("user"); r == protocol.ResultOK && u == "bob" {
		req.Fields.Set("nologin", "y")
	}
	return r, err
}

// authService serves chain over the real wire and returns the proxy's view of it.
func authService(t *testing.T, chain ...protocol.Passdb) AuthService {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go protocol.NewServer(chain).Serve(ctx, ln) //nolint:errcheck
	t.Cleanup(func() { cancel(); ln.Close() })  //nolint:errcheck
	cl, err := authclient.Dial(ln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() }) //nolint:errcheck
	return NewAuthService(cl)
}

func oauthChain(t *testing.T) []protocol.Passdb {
	t.Helper()
	db, err := oauth2.NewPassdb(oauth2.PassdbConfig{Validator: tokenValidator{}})
	if err != nil {
		t.Fatal(err)
	}
	return []protocol.Passdb{barred{db}}
}

// bearerGet sends one request with token and returns its status and headers.
func bearerGet(t *testing.T, base, token string) (int, http.Header) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/.well-known/jmap", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() //nolint:errcheck
	return resp.StatusCode, resp.Header
}

// Bearer is a token login in the auth service; any refusal, nologin included,
// is invalid_token, so the answer does not tell a valid token apart (#2228).
func TestBearerGoesThroughTheAuthService(t *testing.T) {
	svc := authService(t, oauthChain(t)...)
	base := startProxy(t, Options{Auth: svc, Tokens: svc})
	for _, tc := range []struct {
		name, token string
		user        string // "" for a refusal
	}{
		{"valid token", "good-token", "alice"},
		{"invalid token", "bad-token", ""},
		{"valid token, login disabled", "barred-token", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, hdr := bearerGet(t, base, tc.token)
			if tc.user != "" {
				if status != http.StatusOK || hdr.Get("Echo-"+hdrUser) != tc.user {
					t.Fatalf("status %d, backend saw %q, want 200 as %s", status, hdr.Get("Echo-"+hdrUser), tc.user)
				}
				return
			}
			if status != http.StatusUnauthorized {
				t.Fatalf("status %d, want 401", status)
			}
			if !strings.Contains(strings.Join(hdr.Values("WWW-Authenticate"), "|"), `error="invalid_token"`) {
				t.Errorf("WWW-Authenticate %q, want invalid_token", hdr.Values("WWW-Authenticate"))
			}
		})
	}
}

// Bearer is offered exactly when the auth service runs OAUTHBEARER.
func TestBearerIsOfferedOnlyWhenTheServiceRunsIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		chain func(*testing.T) []protocol.Passdb
		offer bool
	}{
		{"with a token passdb", oauthChain, true},
		{"without one", func(*testing.T) []protocol.Passdb { return []protocol.Passdb{passwordOnly{}} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := authService(t, tc.chain(t)...)
			base := startProxy(t, Options{Auth: svc, Tokens: svc})
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/.well-known/jmap", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close() //nolint:errcheck
			got := strings.Join(resp.Header.Values("WWW-Authenticate"), "|")
			if strings.Contains(got, "Bearer") != tc.offer || !strings.Contains(got, `Basic realm="jmap"`) {
				t.Errorf("WWW-Authenticate %q, Bearer offered want %v", got, tc.offer)
			}
		})
	}
}

// passwordOnly is a passdb that runs no tokens, so OAUTHBEARER is not announced.
type passwordOnly struct{}

func (passwordOnly) Authenticate(*protocol.Request) (protocol.Result, error) {
	return protocol.ResultNext, nil
}

// A passdb outage is not a refusal: the client retries, it is not re-prompted.
func TestAnAuthOutageIsServiceUnavailable(t *testing.T) {
	base := startProxy(t, Options{Auth: outage{}, Tokens: outage{}})
	for _, auth := range []string{basic("u1", "pw"), "Bearer good-token"} {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/.well-known/jmap", nil)
		req.Header.Set("Authorization", auth)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close() //nolint:errcheck
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s: status %d, want 503", strings.Fields(auth)[0], resp.StatusCode)
		}
	}
}

type outage struct{}

func (outage) Authenticate(string, string, string, string, string, string) (Login, error) {
	return Login{}, classify(authclient.ErrTempFail)
}
func (outage) AuthenticateToken(string, string, string, string, string) (Login, error) {
	return Login{}, classify(authclient.ErrTempFail)
}
func (outage) OffersTokens() bool { return true }

// peerConn is a connection from a fixed remote address.
type peerConn struct {
	net.Conn
	ip string
}

func (c peerConn) RemoteAddr() net.Addr { return &net.TCPAddr{IP: net.ParseIP(c.ip), Port: 40000} }
func (peerConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("198.51.100.1"), Port: 8443}
}

// Basic and Bearer follow the login proxies' cleartext rule: refused when the
// switch is off and the request is neither over TLS nor from a trusted peer.
func TestCleartextCredentialsFollowTheLoginRule(t *testing.T) {
	svc := authService(t, oauthChain(t)...)
	_, trusted, _ := net.ParseCIDR("10.0.0.0/8")
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("u1:pw"))
	for _, tc := range []struct {
		name          string
		requestTLS    bool
		peer          string // "" leaves the request without a tracked connection
		allow         bool
		authorization string
		refused       bool
	}{
		{"basic, plain request", false, "", false, basic, true},
		{"bearer, plain request", false, "", false, "Bearer good-token", true},
		{"basic, untrusted peer", false, "192.0.2.1", false, basic, true},
		{"basic, TLS request", true, "", false, basic, false},
		{"bearer, TLS request", true, "", false, "Bearer good-token", false},
		{"basic, trusted peer", false, "10.1.2.3", false, basic, false},
		{"bearer, trusted peer", false, "10.1.2.3", false, "Bearer good-token", false},
		{"basic, cleartext allowed", false, "", true, basic, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{Auth: fakeAuth{users: map[string]string{"u1": "pw"}}, Tokens: svc,
				DisablePlainAuth: !tc.allow, TrustedNets: []*net.IPNet{trusted}}
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/jmap/session", nil)
			r.RemoteAddr = "192.0.2.1:40000"
			if tc.peer != "" {
				r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, &connState{conn: peerConn{ip: tc.peer}}))
			}
			if tc.requestTLS {
				r.TLS = &tls.ConnectionState{}
			}
			r.Header.Set("Authorization", tc.authorization)
			_, err := New(opts).authenticate(r, "192.0.2.1", "192.0.2.1", "sid")
			if refused := errors.Is(err, errUnauthorized) || errors.Is(err, errBadToken); refused != tc.refused {
				t.Errorf("refused = %v (err %v), want %v", refused, err, tc.refused)
			}
		})
	}
}
