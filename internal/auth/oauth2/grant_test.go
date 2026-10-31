package oauth2

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/auth/protocol"
)

// The token endpoint's answer decides between a wrong password and an outage.
func TestGrantExchange(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantErr   error
		wantToken string
	}{
		{"token issued", 200, `{"access_token":"tok","token_type":"Bearer","email":"u1@d00001.test"}`, nil, "tok"},
		{"invalid_grant is a wrong password", 401, `{"error":"invalid_grant"}`, ErrGrantRefused, ""},
		{"invalid_grant on 400 too", 400, `{"error":"invalid_grant","error_description":"x"}`, ErrGrantRefused, ""},
		{"invalid_client is an outage", 401, `{"error":"invalid_client"}`, ErrUpstream, ""},
		{"server error", 500, `oops`, ErrUpstream, ""},
		{"success without a token", 200, `{"token_type":"Bearer"}`, ErrUpstream, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var form url.Values
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r.ParseForm() //nolint:errcheck
				form = r.PostForm
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body)) //nolint:errcheck
			}))
			t.Cleanup(srv.Close)
			g, err := NewGrant(GrantConfig{URL: srv.URL, ClientID: "yarilo-auth", ClientSecret: "s3",
				Scopes: []string{"openid", "email"}, HTTPClient: srv.Client()})
			if err != nil {
				t.Fatal(err)
			}
			tok, reply, err := g.Exchange(context.Background(), "u1@d00001.test", "pw&=1")
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil || tok != tt.wantToken || reply.Username != "u1@d00001.test" {
				t.Fatalf("Exchange = %q, %+v, %v", tok, reply, err)
			}
			want := url.Values{"grant_type": {"password"}, "username": {"u1@d00001.test"}, "password": {"pw&=1"},
				"client_id": {"yarilo-auth"}, "client_secret": {"s3"}, "scope": {"openid email"}}
			if form.Encode() != want.Encode() {
				t.Errorf("form = %s, want %s", form.Encode(), want.Encode())
			}
		})
	}
}

type recordingValidator struct {
	tokens []string
	claims *Claims
	err    error
}

func (r *recordingValidator) Validate(_ context.Context, token string) (*Claims, error) {
	r.tokens = append(r.tokens, token)
	return r.claims, r.err
}

func grantServer(t *testing.T, body string, status int) *Grant {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(body)) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	g, err := NewGrant(GrantConfig{URL: srv.URL, ClientID: "c", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// A plaintext login goes through the grant; the reply decides unless it does
// not name the user or Force is on, and a bearer token never reaches the grant.
func TestPassdbGrantAndSettle(t *testing.T) {
	named := `{"access_token":"granted","email":"u1@d00001.test"}`
	anon := `{"access_token":"granted"}`
	user := &Claims{Username: "u1@d00001.test", Extra: map[string]string{"mail_enabled": "yes"}}
	tests := []struct {
		name         string
		grantBody    string
		grantStatus  int
		bearer       bool
		force        bool
		active       string
		want         protocol.Result
		wantSettle   []string
		wantPrimary  []string
		settleClaims *Claims
	}{
		{"reply names the user", named, 200, false, false, "", protocol.ResultOK, nil, nil, user},
		{"reply without the user is settled", anon, 200, false, false, "", protocol.ResultOK, []string{"granted"}, nil, user},
		{"force settles a named reply", named, 200, false, true, "", protocol.ResultOK, []string{"granted"}, nil, user},
		{"active attribute missing from the reply", named, 200, false, false, "mail_enabled", protocol.ResultOK, []string{"granted"}, nil, user},
		{"settle refuses", anon, 200, false, false, "", protocol.ResultNext, []string{"granted"}, nil, &Claims{Username: "u2@d00001.test"}},
		{"wrong password", `{"error":"invalid_grant"}`, 401, false, false, "", protocol.ResultNext, nil, nil, user},
		{"IdP down", `oops`, 503, false, false, "", protocol.ResultTempFail, nil, nil, user},
		{"bearer token skips the grant", `{"error":"invalid_grant"}`, 401, true, false, "", protocol.ResultOK, nil, []string{"pw-or-token"}, user},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settle := &recordingValidator{claims: tt.settleClaims}
			primary := &recordingValidator{claims: user}
			p, err := NewPassdb(PassdbConfig{Validator: primary, Grant: grantServer(t, tt.grantBody, tt.grantStatus),
				Settle: settle, Force: tt.force, ActiveAttribute: tt.active})
			if err != nil {
				t.Fatal(err)
			}
			req := &protocol.Request{Username: "u1@d00001.test", Password: "pw-or-token", Bearer: tt.bearer}
			got, _ := p.Authenticate(req)
			if got != tt.want {
				t.Errorf("result = %v, want %v", got, tt.want)
			}
			if !equalStrings(settle.tokens, tt.wantSettle) || !equalStrings(primary.tokens, tt.wantPrimary) {
				t.Errorf("settle saw %v, primary saw %v; want %v and %v", settle.tokens, primary.tokens, tt.wantSettle, tt.wantPrimary)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
