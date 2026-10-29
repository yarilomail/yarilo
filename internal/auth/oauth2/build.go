package oauth2

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/auth/protocol"
	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// BuildPassdbs constructs one Passdb per configured OAuth2Entry.
// Returns the list in entry order so the configuration's intent
// (which provider has chain priority) is preserved.
//
// Validation failures at construction (missing required URL,
// unknown mode, JWKS fetch failure) propagate as errors so the
// operator notices misconfiguration at startup rather than at
// first login.
func BuildPassdbs(ctx context.Context, entries []config.OAuth2Entry) ([]protocol.Passdb, error) {
	out := make([]protocol.Passdb, 0, len(entries))
	for i, e := range entries {
		pdb, err := buildOne(ctx, e)
		if err != nil {
			return nil, fmt.Errorf("oauth2 entry #%d (%s): %w", i, e.Mode, err)
		}
		out = append(out, pdb)
	}
	return out, nil
}

func buildOne(ctx context.Context, e config.OAuth2Entry) (protocol.Passdb, error) {
	timeout := time.Duration(e.HTTPTimeoutMs) * time.Millisecond
	grace := time.Duration(e.TokenExpireGraceSeconds) * time.Second
	hc, err := NewHTTPClient(timeout, e.TLSCA)
	if err != nil {
		return nil, err
	}
	introspection := func(url string) (Validator, error) {
		return NewIntrospectionValidator(IntrospectionConfig{
			URL:               url,
			Mode:              introMode(e.IntrospectionMode),
			ClientID:          e.ClientID,
			ClientSecret:      e.ClientSecret,
			Issuers:           e.Issuers,
			Audience:          e.Audience,
			RequiredScopes:    e.Scopes,
			UsernameAttribute: e.UsernameAttribute,
			ExpireGrace:       grace,
			HTTPTimeout:       timeout,
			HTTPClient:        hc,
		})
	}

	var validator Validator
	switch e.Mode {
	case config.OAuth2ModeLocalJWT:
		keys := make([]LocalKey, len(e.LocalKeys))
		for i, k := range e.LocalKeys {
			keys[i] = LocalKey{Azp: k.Azp, Alg: k.Alg, Kid: k.Kid, File: k.File}
		}
		validator, err = NewLocalJWTValidator(ctx, LocalJWTConfig{
			JWKSURL:            e.JWKSURL,
			Keys:               keys,
			HTTPClient:         hc,
			UnknownKIDInterval: time.Duration(e.JWKSUnknownKIDIntervalSeconds) * time.Second,
			Issuers:            e.Issuers,
			Audience:           e.Audience,
			RequiredScopes:     e.Scopes,
			UsernameAttribute:  e.UsernameAttribute,
			ExpireGrace:        grace,
			HTTPTimeout:        timeout,
		})
	case config.OAuth2ModeIntrospection:
		validator, err = introspection(e.IntrospectionURL)
	case config.OAuth2ModeTokeninfo:
		validator, err = NewTokeninfoValidator(TokeninfoConfig{
			URL:               e.TokeninfoURL,
			Issuers:           e.Issuers,
			Audience:          e.Audience,
			RequiredScopes:    e.Scopes,
			UsernameAttribute: e.UsernameAttribute,
			ExpireGrace:       grace,
			HTTPTimeout:       timeout,
			HTTPClient:        hc,
		})
	case config.OAuth2ModeDiscovery:
		validator, err = NewDiscoveryValidator(ctx, DiscoveryConfig{
			IssuerURL:           e.IssuerURL,
			PreferIntrospection: e.PreferIntrospection,
			IntrospectionMode:   introMode(e.IntrospectionMode),
			ClientID:            e.ClientID,
			ClientSecret:        e.ClientSecret,
			Issuers:             e.Issuers,
			Audience:            e.Audience,
			RequiredScopes:      e.Scopes,
			UsernameAttribute:   e.UsernameAttribute,
			ExpireGrace:         grace,
			HTTPTimeout:         timeout,
			HTTPClient:          hc,
			UnknownKIDInterval:  time.Duration(e.JWKSUnknownKIDIntervalSeconds) * time.Second,
		})
	default:
		return nil, fmt.Errorf("oauth2: unknown mode %q (valid: local | introspection | tokeninfo | discovery)", e.Mode)
	}
	if err != nil {
		return nil, err
	}
	// A tokeninfo or grant reply that does not name the user is decided by the
	// entry's validator, or by introspection after tokeninfo (#2231).
	settle := validator
	if e.Mode == config.OAuth2ModeTokeninfo {
		settle = nil
		if e.IntrospectionURL != "" {
			if settle, err = introspection(e.IntrospectionURL); err != nil {
				return nil, err
			}
		}
	}
	var grant *Grant
	if e.GrantURL != "" {
		grant, err = NewGrant(GrantConfig{URL: e.GrantURL, ClientID: e.ClientID, ClientSecret: e.ClientSecret,
			Scopes: e.Scopes, UsernameAttribute: e.UsernameAttribute, HTTPClient: hc})
		if err != nil {
			return nil, err
		}
	}
	return NewPassdb(PassdbConfig{
		Grant:                  grant,
		Settle:                 settle,
		SettleValidator:        e.Mode == config.OAuth2ModeTokeninfo,
		Force:                  e.ForceIntrospection,
		Validator:              validator,
		UsernameTemplate:       e.UsernameValidationFormat,
		ActiveAttribute:        e.ActiveAttribute,
		ActiveValue:            e.ActiveValue,
		ExtraFields:            e.ExtraFields,
		Scope:                  strings.Join(e.Scopes, " "),
		OpenIDConfigurationURL: discoveryURL(e.IssuerURL),
	})
}

// discoveryURL is the issuer's OpenID configuration document, "" without one.
func discoveryURL(issuer string) string {
	if issuer == "" {
		return ""
	}
	return strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
}

func introMode(raw string) IntrospectionMode {
	switch raw {
	case "auth":
		return IntrospectionAuthBearer
	case "get":
		return IntrospectionGet
	case "", "post":
		return IntrospectionPostForm
	default:
		// Unknown values caught by NewIntrospectionValidator.
		return IntrospectionMode(raw)
	}
}
