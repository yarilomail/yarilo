package oauth2

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func keyFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func pubPEM(t *testing.T, pub any) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

type tokenSpec struct {
	method jwt.SigningMethod
	kid    any // nil: no kid header
	azp    string
	key    any
}

func signSpec(t *testing.T, s tokenSpec) string {
	t.Helper()
	claims := jwt.MapClaims{"email": "u1@d00001.test", "exp": time.Now().Add(time.Minute).Unix()}
	if s.azp != "" {
		claims["azp"] = s.azp
	}
	tok := jwt.NewWithClaims(s.method, claims)
	if s.kid != nil {
		tok.Header["kid"] = s.kid
	}
	out, err := tok.SignedString(s.key)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A token is checked with the key its azp, alg and kid name, and with no other.
func TestLocalKeysPickTheKey(t *testing.T) {
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	edPub, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	secret := []byte("0123456789abcdef0123456789abcdef")
	other := []byte("fedcba9876543210fedcba9876543210")
	hsFile := keyFile(t, base64.RawURLEncoding.EncodeToString(secret)+"\n")
	rsaFile := keyFile(t, pubPEM(t, &rsaKey.PublicKey))

	tests := []struct {
		name string
		keys []LocalKey
		tok  tokenSpec
		ok   bool
	}{
		{"HS256 by kid", []LocalKey{{Alg: "HS256", Kid: "k1", File: hsFile}},
			tokenSpec{jwt.SigningMethodHS256, "k1", "", secret}, true},
		{"HS256 without kid is the default key", []LocalKey{{Alg: "HS256", File: hsFile}},
			tokenSpec{jwt.SigningMethodHS256, nil, "", secret}, true},
		{"HS256 signed with another secret", []LocalKey{{Alg: "HS256", Kid: "k1", File: hsFile}},
			tokenSpec{jwt.SigningMethodHS256, "k1", "", other}, false},
		{"azp selects its key", []LocalKey{{Azp: "app", Alg: "HS256", Kid: "k1", File: hsFile}},
			tokenSpec{jwt.SigningMethodHS256, "k1", "app", secret}, true},
		{"another azp does not reach that key", []LocalKey{{Azp: "app", Alg: "HS256", Kid: "k1", File: hsFile}},
			tokenSpec{jwt.SigningMethodHS256, "k1", "other", secret}, false},
		{"empty kid header is refused", []LocalKey{{Alg: "HS256", Kid: "", File: hsFile}},
			tokenSpec{jwt.SigningMethodHS256, "", "", secret}, false},
		{"PS256", []LocalKey{{Alg: "PS256", Kid: "p", File: rsaFile}},
			tokenSpec{jwt.SigningMethodPS256, "p", "", rsaKey}, true},
		{"alg is part of the key's name", []LocalKey{{Alg: "PS256", Kid: "p", File: rsaFile}},
			tokenSpec{jwt.SigningMethodRS256, "p", "", rsaKey}, false},
		{"ES256", []LocalKey{{Alg: "ES256", Kid: "e", File: keyFile(t, pubPEM(t, &ecKey.PublicKey))}},
			tokenSpec{jwt.SigningMethodES256, "e", "", ecKey}, true},
		{"EdDSA", []LocalKey{{Alg: "EdDSA", Kid: "d", File: keyFile(t, pubPEM(t, edPub))}},
			tokenSpec{jwt.SigningMethodEdDSA, "d", "", edPriv}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := NewLocalJWTValidator(context.Background(), LocalJWTConfig{Keys: tt.keys})
			if err != nil {
				t.Fatalf("NewLocalJWTValidator: %v", err)
			}
			c, err := v.Validate(context.Background(), signSpec(t, tt.tok))
			if tt.ok && (err != nil || c.Username != "u1@d00001.test") {
				t.Fatalf("Validate = %v, %v; want the token accepted", c, err)
			}
			if !tt.ok && !errors.Is(err, ErrTokenInvalid) {
				t.Fatalf("Validate err = %v, want ErrTokenInvalid", err)
			}
		})
	}
}

// An oct key in the JWKS must not verify an HS token: shared secrets come from
// the operator's files only (#2231).
func TestLocalKeysHSNeverFromTheJWKS(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	body, _ := json.Marshal(map[string]any{"keys": []map[string]any{{
		"kty": "oct", "alg": "HS256", "use": "sig", "kid": "oct1",
		"k": base64.RawURLEncoding.EncodeToString(secret),
	}}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)
	v, err := NewLocalJWTValidator(context.Background(), LocalJWTConfig{JWKSURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.Validate(context.Background(), signSpec(t, tokenSpec{jwt.SigningMethodHS256, "oct1", "", secret}))
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("Validate err = %v, want the HS token refused", err)
	}
}

// With local keys configured, a token they do not name still goes to the JWKS.
func TestLocalKeysFallBackToTheJWKS(t *testing.T) {
	h := newJWKSHarness(t)
	v, err := NewLocalJWTValidator(context.Background(), LocalJWTConfig{
		JWKSURL: h.server.URL,
		Keys:    []LocalKey{{Alg: "HS256", Kid: "k1", File: keyFile(t, "c2VjcmV0")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := v.Validate(context.Background(), h.sign(t, jwt.MapClaims{"email": "u1@d00001.test",
		"exp": time.Now().Add(time.Minute).Unix()}))
	if err != nil || c.Username != "u1@d00001.test" {
		t.Fatalf("Validate = %v, %v; want the JWKS key to verify it", c, err)
	}
}

// A key that cannot sign its alg is refused at startup, not at the first login.
func TestLocalKeysRefusedAtLoad(t *testing.T) {
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	edPub, _, _ := ed25519.GenerateKey(rand.Reader)
	tests := []struct {
		name, alg, body, want string
	}{
		{"curve does not match the alg", "ES384", pubPEM(t, &ecKey.PublicKey), "does not sign ES384"},
		{"ed25519 key for RS256", "RS256", pubPEM(t, edPub), "does not sign RS256"},
		{"RSA key for EdDSA", "EdDSA", pubPEM(t, &rsaKey.PublicKey), "does not sign EdDSA"},
		{"HS secret not base64", "HS256", "not base64 !!", "not a base64 shared secret"},
		{"no PUBLIC KEY block", "RS256", "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n", "no PEM PUBLIC KEY block"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewLocalJWTValidator(context.Background(), LocalJWTConfig{
				Keys: []LocalKey{{Alg: tt.alg, File: keyFile(t, tt.body)}},
			})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to say %q", err, tt.want)
			}
		})
	}
	_, err := NewLocalJWTValidator(context.Background(), LocalJWTConfig{
		Keys: []LocalKey{{Alg: "RS256", File: filepath.Join(t.TempDir(), "missing")}},
	})
	if err == nil {
		t.Fatal("a missing key file was accepted")
	}
}
