package oauth2

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/yarilomail/yarilo/v2/internal/auth/protocol"
	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// idp is a TLS identity provider whose certificate no system root signs.
type idp struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	caFile string
	hits   map[string]int
}

func newIDP(t *testing.T) *idp {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &idp{key: key, hits: map[string]int{}}
	jwks, _ := json.Marshal(map[string]any{"keys": []map[string]any{{"kty": "RSA", "use": "sig", "alg": "RS256",
		"kid": "test-kid", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}})
	mux := http.NewServeMux()
	reply := func(path string, body func() []byte) {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			p.hits[path]++
			w.Header().Set("Content-Type", "application/json")
			w.Write(body()) //nolint:errcheck
		})
	}
	reply("/certs", func() []byte { return jwks })
	reply("/introspect", func() []byte { return []byte(`{"active":true,"email":"u1@d00001.test"}`) })
	reply("/tokeninfo", func() []byte { return []byte(`{"email":"u1@d00001.test"}`) })
	reply("/tokeninfo-anon", func() []byte { return []byte(`{"sub":"x"}`) })
	reply("/token", func() []byte { return []byte(`{"access_token":"` + p.token(t) + `"}`) })
	reply("/.well-known/openid-configuration", func() []byte {
		return []byte(`{"issuer":"` + p.srv.URL + `","jwks_uri":"` + p.srv.URL + `/certs"}`)
	})
	p.srv = httptest.NewTLSServer(mux)
	t.Cleanup(p.srv.Close)
	p.caFile = keyFile(t, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.srv.Certificate().Raw})))
	return p
}

func (p *idp) token(t *testing.T) string {
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"email": "u1@d00001.test", "iss": p.srv.URL,
		"exp": time.Now().Add(time.Minute).Unix()})
	tok.Header["kid"] = "test-kid"
	s, err := tok.SignedString(p.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func buildOneEntry(t *testing.T, e config.OAuth2Entry) *Passdb {
	t.Helper()
	e.HTTPTimeoutMs, e.TokenExpireGraceSeconds, e.UsernameAttribute = 2000, 60, "email"
	pdbs, err := BuildPassdbs(context.Background(), []config.OAuth2Entry{e})
	if err != nil {
		t.Fatalf("BuildPassdbs: %v", err)
	}
	return pdbs[0].(*Passdb)
}

// Every call an entry makes trusts oauth2_tls_ca; without it the same IdP is
// unreachable, so each row proves its own path carries the client (#2231).
func TestBuildTrustsTheEntryCA(t *testing.T) {
	p := newIDP(t)
	u := p.srv.URL
	tests := []struct {
		name   string
		entry  config.OAuth2Entry
		bearer bool
	}{
		{"introspection", config.OAuth2Entry{Mode: config.OAuth2ModeIntrospection, IntrospectionURL: u + "/introspect"}, true},
		{"tokeninfo", config.OAuth2Entry{Mode: config.OAuth2ModeTokeninfo, TokeninfoURL: u + "/tokeninfo"}, true},
		{"local JWKS", config.OAuth2Entry{Mode: config.OAuth2ModeLocalJWT, JWKSURL: u + "/certs"}, true},
		{"discovery", config.OAuth2Entry{Mode: config.OAuth2ModeDiscovery, IssuerURL: u}, true},
		{"grant", config.OAuth2Entry{Mode: config.OAuth2ModeLocalJWT, JWKSURL: u + "/certs",
			GrantURL: u + "/token", ClientID: "c"}, false},
	}
	for _, tt := range tests {
		for _, withCA := range []bool{true, false} {
			name := tt.name + map[bool]string{true: " with tls_ca", false: " on system roots"}[withCA]
			t.Run(name, func(t *testing.T) {
				e := tt.entry
				if withCA {
					e.TLSCA = p.caFile
				}
				pdbs, err := BuildPassdbs(context.Background(), []config.OAuth2Entry{func() config.OAuth2Entry {
					e.HTTPTimeoutMs, e.TokenExpireGraceSeconds, e.UsernameAttribute = 2000, 60, "email"
					return e
				}()})
				if err != nil {
					if withCA {
						t.Fatalf("BuildPassdbs: %v", err)
					}
					return // discovery fetches at build: refused there
				}
				password := "secret"
				if tt.bearer {
					password = p.token(t)
				}
				got, _ := pdbs[0].Authenticate(&protocol.Request{Username: "u1@d00001.test", Password: password, Bearer: tt.bearer})
				if withCA && got != protocol.ResultOK {
					t.Fatalf("result = %v, want OK through the entry's CA", got)
				}
				if !withCA && got == protocol.ResultOK {
					t.Fatal("logged in although the IdP's CA is not trusted")
				}
			})
		}
	}
}

// After a tokeninfo reply that does not name the user, introspection decides;
// with force it decides even a named one.
func TestBuildTokeninfoSettlesThroughIntrospection(t *testing.T) {
	p := newIDP(t)
	tests := []struct {
		name, tokeninfo string
		force           bool
		wantIntrospect  int
	}{
		{"named reply stands", "/tokeninfo", false, 0},
		{"anonymous reply goes to introspection", "/tokeninfo-anon", false, 1},
		{"force sends a named reply to introspection", "/tokeninfo", true, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := p.hits["/introspect"]
			pdb := buildOneEntry(t, config.OAuth2Entry{Mode: config.OAuth2ModeTokeninfo, TokeninfoURL: p.srv.URL + tt.tokeninfo,
				IntrospectionURL: p.srv.URL + "/introspect", ForceIntrospection: tt.force, TLSCA: p.caFile})
			got, _ := pdb.Authenticate(&protocol.Request{Username: "u1@d00001.test", Password: "opaque", Bearer: true})
			if got != protocol.ResultOK || p.hits["/introspect"]-before != tt.wantIntrospect {
				t.Fatalf("result %v, introspections %d; want OK and %d", got, p.hits["/introspect"]-before, tt.wantIntrospect)
			}
		})
	}
}
