package oauth2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ErrGrantRefused is the token endpoint's invalid_grant: a wrong password,
// not an outage.
var ErrGrantRefused = errors.New("oauth2: password grant refused")

// GrantConfig configures a password grant (RFC 6749 §4.3).
type GrantConfig struct {
	URL, ClientID, ClientSecret string
	Scopes                      []string
	UsernameAttribute           string
	HTTPClient                  *http.Client
}

// Grant exchanges a plaintext login's password for an access token.
type Grant struct {
	cfg GrantConfig
}

// NewGrant constructs the exchange; URL and HTTPClient are required.
func NewGrant(cfg GrantConfig) (*Grant, error) {
	if cfg.URL == "" || cfg.HTTPClient == nil {
		return nil, errors.New("oauth2/grant: URL and HTTPClient are required")
	}
	if cfg.UsernameAttribute == "" {
		cfg.UsernameAttribute = "email"
	}
	return &Grant{cfg: cfg}, nil
}

// Exchange returns the access token and the reply's fields as Claims.
// invalid_grant is ErrGrantRefused; anything else that fails is ErrUpstream.
func (g *Grant) Exchange(ctx context.Context, username, password string) (string, *Claims, error) {
	form := url.Values{"grant_type": {"password"}, "username": {username}, "password": {password}}
	if g.cfg.ClientID != "" {
		form.Set("client_id", g.cfg.ClientID)
	}
	if g.cfg.ClientSecret != "" {
		form.Set("client_secret", g.cfg.ClientSecret)
	}
	if len(g.cfg.Scopes) > 0 {
		form.Set("scope", strings.Join(g.cfg.Scopes, " "))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.URL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", nil, fmt.Errorf("%w: build request: %v", ErrUpstream, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := g.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", nil, fmt.Errorf("%w: read body: %v", ErrUpstream, err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", nil, fmt.Errorf("%w: grant HTTP %d, unparsable reply", ErrUpstream, resp.StatusCode)
	}
	if resp.StatusCode/100 != 2 {
		if raw["error"] == "invalid_grant" {
			return "", nil, ErrGrantRefused
		}
		return "", nil, fmt.Errorf("%w: grant HTTP %d, error %v", ErrUpstream, resp.StatusCode, raw["error"])
	}
	token, _ := raw["access_token"].(string)
	if token == "" {
		return "", nil, fmt.Errorf("%w: grant reply has no access_token", ErrUpstream)
	}
	return token, introspectionToClaims(raw, g.cfg.UsernameAttribute), nil
}
