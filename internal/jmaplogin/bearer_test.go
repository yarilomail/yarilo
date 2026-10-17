package jmaplogin

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"

	authclient "github.com/yarilomail/yarilo/internal/auth/client"
	"github.com/yarilomail/yarilo/internal/auth/oauth2"
	"github.com/yarilomail/yarilo/internal/auth/protocol"
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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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

func (outage) Authenticate(string, string, string, string, string, string) (string, error) {
	return "", classify(authclient.ErrTempFail)
}
func (outage) AuthenticateToken(string, string, string, string, string) (string, error) {
	return "", classify(authclient.ErrTempFail)
}
func (outage) OffersTokens() bool { return true }

// With cleartext refused a token over plain HTTP is refused like a password.
func TestABearerOverCleartextIsRefusedWhenPasswordsAre(t *testing.T) {
	svc := authService(t, oauthChain(t)...)
	base := startProxy(t, Options{Auth: svc, Tokens: svc, DisablePlainAuth: true})
	if status, _ := bearerGet(t, base, "good-token"); status != http.StatusUnauthorized {
		t.Errorf("a valid token over plain HTTP answered %d, want 401", status)
	}
}
