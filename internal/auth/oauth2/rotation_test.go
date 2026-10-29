package oauth2

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/yarilomail/yarilo/v2/internal/auth/protocol"
	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// rotatingIdP serves one RSA key at a time and can rotate to a new kid.
type rotatingIdP struct {
	mu  sync.Mutex
	key *rsa.PrivateKey
	kid string
	srv *httptest.Server
}

func newRotatingIdP(t *testing.T) *rotatingIdP {
	t.Helper()
	p := &rotatingIdP{}
	p.rotate(t, "k1")
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		body, _ := json.Marshal(map[string]any{"keys": []map[string]any{{"kty": "RSA", "use": "sig", "alg": "RS256",
			"kid": p.kid, "n": base64.RawURLEncoding.EncodeToString(p.key.N.Bytes()), "e": "AQAB"}}})
		w.Write(body) //nolint:errcheck
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *rotatingIdP) rotate(t *testing.T, kid string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.key, p.kid = key, kid
	p.mu.Unlock()
}

func (p *rotatingIdP) token(t *testing.T, iss, kid string) string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": iss, "email": "u1@d00001.test",
		"exp": time.Now().Add(time.Minute).Unix()})
	tok.Header["kid"] = kid
	s, err := tok.SignedString(p.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A token of a refused issuer is refused before any key lookup, so it does not
// spend the one unknown-kid refresh a rotation needs (#2249).
func TestAForeignTokenDoesNotBlockRotation(t *testing.T) {
	idp := newRotatingIdP(t)
	v, err := NewLocalJWTValidator(context.Background(), LocalJWTConfig{JWKSURL: idp.srv.URL,
		Issuers: []string{"https://idp"}, UnknownKIDInterval: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Validate(context.Background(), idp.token(t, "https://idp", "k1")); err != nil {
		t.Fatalf("token under the first key: %v", err)
	}
	if _, err := v.Validate(context.Background(), idp.token(t, "https://other", "forged")); err == nil {
		t.Fatal("a foreign issuer's token was accepted")
	}
	idp.rotate(t, "k2")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := v.Validate(ctx, idp.token(t, "https://idp", "k2")); err != nil {
		t.Fatalf("token under the rotated key after a foreign one: %v", err)
	}
}

// Interval 0 never refreshes for an unknown kid; the periodic refresh still does.
func TestUnknownKIDRefreshOff(t *testing.T) {
	idp := newRotatingIdP(t)
	v, err := NewLocalJWTValidator(context.Background(), LocalJWTConfig{JWKSURL: idp.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	idp.rotate(t, "k2")
	if _, err := v.Validate(context.Background(), idp.token(t, "", "k2")); err == nil {
		t.Fatal("an unknown kid refreshed the JWKS with the refresh turned off")
	}
}

// The entry's interval reaches the JWKS client: 0 keeps a rotated kid out,
// the default lets its first refresh through.
func TestBuildPassesTheUnknownKIDInterval(t *testing.T) {
	for _, tc := range []struct {
		secs int
		ok   bool
	}{{0, false}, {300, true}} {
		idp := newRotatingIdP(t)
		pdbs, err := BuildPassdbs(context.Background(), []config.OAuth2Entry{{Mode: config.OAuth2ModeLocalJWT, JWKSURL: idp.srv.URL,
			HTTPTimeoutMs: 2000, UsernameAttribute: "email", JWKSUnknownKIDIntervalSeconds: tc.secs}})
		if err != nil {
			t.Fatal(err)
		}
		idp.rotate(t, "k2")
		got, _ := pdbs[0].Authenticate(&protocol.Request{Username: "u1@d00001.test", Password: idp.token(t, "", "k2"), Bearer: true})
		if (got == protocol.ResultOK) != tc.ok {
			t.Errorf("interval %ds: result %v, want ok=%v", tc.secs, got, tc.ok)
		}
	}
}
