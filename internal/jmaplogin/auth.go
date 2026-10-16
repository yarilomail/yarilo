package jmaplogin

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// errUnauthorized is returned for every rejected credential. The cause is
// logged, never sent.
var errUnauthorized = errors.New("jmap-login: unauthorized")

// errTemporary is the auth service failing, not the credential.
var errTemporary = errors.New("jmap-login: authentication temporarily unavailable")

// errBadToken is a refused Bearer, answered with RFC 6750's invalid_token
// whatever refused it: telling a valid token apart would be an oracle.
var errBadToken = fmt.Errorf("%w: bearer token refused", errUnauthorized)

// authenticate resolves the request's credentials to a username. Bearer is
// tried first and a rejected token is never retried as Basic, which would
// report a token failure as a password one. No penalty accounting here:
// yarilo-auth applies it for every caller, and a second one double-counts.
func (s *Server) authenticate(r *http.Request, clientIP, sessionID string) (string, error) {
	scheme, cred, ok := parseAuthorization(r.Header.Get("Authorization"))
	if !ok {
		return "", errUnauthorized
	}
	switch strings.ToLower(scheme) {
	case "bearer":
		// A token is a password to whoever reads it: the cleartext rule is Basic's.
		if s.opts.Tokens == nil || (s.opts.DisablePlainAuth && r.TLS == nil) {
			return "", errBadToken
		}
		username, err := s.opts.Tokens.AuthenticateToken(cred, service, clientIP, sessionID)
		if err != nil {
			slog.Info("jmap-login: bearer rejected", "ip", clientIP, "err", err)
			if errors.Is(err, errTemporary) {
				return "", err
			}
			return "", errBadToken
		}
		return username, nil
	case "basic":
		return s.authenticateBasic(r, cred, clientIP, sessionID)
	}
	return "", errUnauthorized
}

func (s *Server) authenticateBasic(r *http.Request, cred, clientIP, sessionID string) (string, error) {
	if s.opts.Auth == nil {
		return "", errUnauthorized
	}
	// Plaintext credentials on a cleartext connection are refused outright
	// rather than accepted and logged, matching the other protocols.
	if s.opts.DisablePlainAuth && r.TLS == nil {
		return "", errUnauthorized
	}
	raw, err := base64.StdEncoding.DecodeString(cred)
	if err != nil {
		return "", errUnauthorized
	}
	username, password, found := strings.Cut(string(raw), ":")
	if !found || username == "" {
		return "", errUnauthorized
	}
	resolved, err := s.opts.Auth.Authenticate(username, password, service, clientIP, sessionID)
	if err != nil {
		slog.Info("jmap-login: basic rejected", "user", username, "ip", clientIP, "err", err)
		if errors.Is(err, errTemporary) {
			return "", err
		}
		return "", errUnauthorized
	}
	if resolved == "" {
		resolved = username
	}
	return resolved, nil
}

// parseAuthorization splits an Authorization header into scheme and credential.
func parseAuthorization(h string) (scheme, cred string, ok bool) {
	scheme, cred, ok = strings.Cut(strings.TrimSpace(h), " ")
	if !ok {
		return "", "", false
	}
	cred = strings.TrimSpace(cred)
	return scheme, cred, cred != ""
}
