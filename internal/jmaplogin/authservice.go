package jmaplogin

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	authclient "github.com/yarilomail/yarilo/internal/auth/client"
	"github.com/yarilomail/yarilo/internal/auth/protocol"
)

// AuthService is the auth service seen by this proxy: Basic as a password
// login, Bearer as OAUTHBEARER, both with this proxy's service name (#2228).
type AuthService struct{ cl *authclient.Client }

// NewAuthService wraps the auth client the binary dialled.
func NewAuthService(cl *authclient.Client) AuthService { return AuthService{cl: cl} }

// Authenticate satisfies PasswordAuthenticator.
func (a AuthService) Authenticate(username, password, svc, remoteIP, localIP, sessionID string) (string, error) {
	res, err := a.cl.Authenticate(username, password, svc, remoteIP, localIP, sessionID)
	if err != nil {
		return "", classify(err)
	}
	return accepted(res)
}

// AuthenticateToken satisfies TokenAuthenticator. The token names the user, so
// the exchange carries no authzid and the service answers with the name.
func (a AuthService) AuthenticateToken(token, svc, remoteIP, localIP, sessionID string) (string, error) {
	if strings.ContainsAny(token, "\x01\r\n") {
		return "", errUnauthorized
	}
	initial := []byte("n,,\x01auth=Bearer " + token + "\x01\x01")
	x, step, err := a.cl.BeginSASL(protocol.MechOAuthBearer, svc, remoteIP, localIP, sessionID, nil, initial)
	if err != nil {
		return "", classify(err)
	}
	if !step.Done {
		// A refused token is answered with an error document first; the
		// acknowledgement ends the exchange (RFC 7628 §3.2.2).
		if step, err = x.Next([]byte{1}); err != nil {
			return "", classify(err)
		}
		if !step.Done {
			x.Cancel()
			return "", errUnauthorized
		}
	}
	return accepted(step.Result)
}

// OffersTokens reports whether the service runs OAUTHBEARER now; the client
// refreshes its mechanisms on every handshake, a reconnect included.
func (a AuthService) OffersTokens() bool {
	return slices.Contains(a.cl.Mechanisms(), protocol.MechOAuthBearer)
}

func accepted(res *authclient.AuthResult) (string, error) {
	if res == nil || res.Username == "" {
		return "", errUnauthorized
	}
	if res.Nologin {
		return "", fmt.Errorf("%w: login disabled for %s", errUnauthorized, res.Username)
	}
	return res.Username, nil
}

// classify keeps a passdb outage apart from a refusal: the client should retry
// the same credential, not be asked for another.
func classify(err error) error {
	if errors.Is(err, authclient.ErrTempFail) {
		return fmt.Errorf("%w: %w", errTemporary, err)
	}
	return fmt.Errorf("%w: %w", errUnauthorized, err)
}
