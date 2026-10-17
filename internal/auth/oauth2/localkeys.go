package oauth2

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// LocalKey is one operator-provisioned verification key; empty Azp or Kid
// stand for "default", as a token without that claim or header does.
type LocalKey struct {
	Azp, Alg, Kid string
	// File holds a PEM public key, or for HS* the base64 shared secret.
	File string
}

type localKeyID struct{ azp, alg, kid string }

func loadLocalKeys(keys []LocalKey) (map[localKeyID]any, error) {
	out := make(map[localKeyID]any, len(keys))
	for _, k := range keys {
		raw, err := os.ReadFile(k.File)
		if err != nil {
			return nil, fmt.Errorf("oauth2/local_keys: %w", err)
		}
		key, err := parseLocalKey(k.Alg, raw)
		if err != nil {
			return nil, fmt.Errorf("oauth2/local_keys: %s (%s): %w", k.File, k.Alg, err)
		}
		out[localKeyID{orDefault(k.Azp), k.Alg, orDefault(k.Kid)}] = key
	}
	return out, nil
}

func parseLocalKey(alg string, raw []byte) (any, error) {
	if strings.HasPrefix(alg, "HS") {
		s := strings.TrimSpace(string(raw))
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			if b, err := enc.DecodeString(s); err == nil && len(b) > 0 {
				return b, nil
			}
		}
		return nil, errors.New("not a base64 shared secret")
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("no PEM PUBLIC KEY block")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ok := false
	switch p := pub.(type) {
	case *rsa.PublicKey:
		ok = strings.HasPrefix(alg, "RS") || strings.HasPrefix(alg, "PS")
	case *ecdsa.PublicKey:
		ok = map[string]elliptic.Curve{"ES256": elliptic.P256(), "ES384": elliptic.P384(), "ES512": elliptic.P521()}[alg] == p.Curve
	case ed25519.PublicKey:
		ok = alg == "EdDSA"
	}
	if !ok {
		return nil, fmt.Errorf("a %T does not sign %s", pub, alg)
	}
	return pub, nil
}

// localKey picks the key by azp claim, alg and kid header; HS* never falls
// back to the JWKS, where a published public key would act as the secret.
func (v *LocalJWTValidator) localKey(t *jwt.Token) (any, error) {
	alg := t.Method.Alg()
	kid := "default"
	if raw, ok := t.Header["kid"]; ok {
		// An empty kid names no key: only an absent one means "default".
		kid, _ = raw.(string)
	}
	azp := "default"
	if mc, ok := t.Claims.(jwt.MapClaims); ok {
		if s, isStr := mc["azp"].(string); isStr && s != "" {
			azp = s
		}
	}
	if k, ok := v.keys[localKeyID{azp, alg, kid}]; ok {
		return k, nil
	}
	if strings.HasPrefix(alg, "HS") || v.kf == nil {
		return nil, fmt.Errorf("no key for azp %q, alg %s, kid %q", azp, alg, kid)
	}
	// A token of an issuer we refuse anyway must not spend a JWKS refresh (#2249).
	if iss, _ := t.Claims.(jwt.MapClaims)["iss"].(string); len(v.cfg.Issuers) > 0 && !containsString(v.cfg.Issuers, iss) {
		return nil, fmt.Errorf("%w: iss=%q allowed=%v", ErrIssuerMismatch, iss, v.cfg.Issuers)
	}
	return v.kf.Keyfunc(t)
}

func orDefault(s string) string {
	if s == "" {
		return "default"
	}
	return s
}
