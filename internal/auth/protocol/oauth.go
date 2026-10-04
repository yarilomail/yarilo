package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
)

// OAuth SASL mechanisms (RFC 7628, and the older XOAUTH2).
const (
	MechOAuthBearer = "OAUTHBEARER"
	MechXOAuth2     = "XOAUTH2"
)

// OAuth2Passdb is a passdb that validates bearer tokens. Its scope and
// discovery URL are what an OAuth refusal tells the client.
type OAuth2Passdb interface {
	OAuth2Failure() (scope, openidConfiguration string)
}

func isOAuth(mech string) bool { return mech == MechOAuthBearer || mech == MechXOAuth2 }

// oauth2Passdb is the chain's token validator, or nil when it has none: then
// the OAuth mechanisms are neither announced nor run.
func (s *Server) oauth2Passdb() OAuth2Passdb {
	for _, db := range s.passdbs {
		if o, ok := db.(OAuth2Passdb); ok {
			return o
		}
	}
	return nil
}

var errOAuthRequest = errors.New("oauth: malformed client response")

// parseOAuthBearer reads gs2-header ^A *(key=value ^A) ^A (RFC 7628 §3.1).
func parseOAuthBearer(msg []byte) (user, token string, err error) {
	header, rest, ok := bytes.Cut(msg, []byte{1})
	if !ok {
		return "", "", errOAuthRequest
	}
	parts := strings.SplitN(string(header), ",", 3)
	if len(parts) != 3 || parts[2] != "" || strings.HasPrefix(parts[0], "p=") ||
		(parts[0] != "n" && parts[0] != "y") || !strings.HasPrefix(parts[1], "a=") {
		return "", "", errOAuthRequest
	}
	user = strings.NewReplacer("=2C", ",", "=3D", "=").Replace(strings.TrimPrefix(parts[1], "a="))
	if user == "" || !bytes.HasSuffix(rest, []byte{1}) {
		return "", "", errOAuthRequest
	}
	return bearer(user, strings.Split(string(rest[:len(rest)-1]), "\x01"))
}

// parseXOAuth2 reads user=... ^A auth=Bearer ... ^A ^A.
func parseXOAuth2(msg []byte) (user, token string, err error) {
	fields := strings.Split(string(msg), "\x01")
	for _, f := range fields {
		if v, ok := strings.CutPrefix(f, "user="); ok {
			user = v
		}
	}
	if user == "" {
		return "", "", errOAuthRequest
	}
	return bearer(user, fields)
}

// errOAuthToken is an auth= field that does not carry a usable bearer token.
var errOAuthToken = errors.New("oauth: not a bearer token")

// bearer finds the auth= field: missing is a malformed request, present but
// not a bearer token is a bad token.
func bearer(user string, fields []string) (string, string, error) {
	for _, f := range fields {
		v, ok := strings.CutPrefix(f, "auth=")
		if !ok {
			continue
		}
		if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") && !strings.ContainsAny(v[7:], "\r\n") {
			return user, v[7:], nil
		}
		return "", "", errOAuthToken
	}
	return "", "", errOAuthRequest
}

// oauthFailure is the JSON a refused OAuth login is answered with, before the
// final FAIL; XOAUTH2 spells the status as an HTTP code.
func oauthFailure(mech, status string, db OAuth2Passdb) []byte {
	scope, openid := db.OAuth2Failure()
	if scope == "" {
		scope = "mail"
	}
	out := struct {
		Status  string `json:"status"`
		Schemes string `json:"schemes,omitempty"`
		Scope   string `json:"scope"`
		OpenID  string `json:"openid-configuration,omitempty"`
	}{Status: status, Scope: scope, OpenID: openid}
	if mech == MechXOAuth2 {
		out.Schemes = "bearer"
		switch status {
		case "invalid_token":
			out.Status = "401"
		case "insufficient_scope":
			out.Status = "403"
		default:
			out.Status = "400"
		}
	}
	b, _ := json.Marshal(out)
	return b
}

// beginOAuth starts an OAuth login. Without an initial response the client is
// sent an empty challenge first.
func (s *Server) beginOAuth(conn net.Conn, live *exchanges, id, mech, service, resp, remoteIP, sessionID string) string {
	db := s.oauth2Passdb()
	if db == nil {
		fmt.Fprintf(conn, "FAIL\t%s\treason=unsupported mechanism\n", id)
		return "bad_request"
	}
	x := &saslExchange{mech: mech, service: service, rip: remoteIP, session: sessionID, oauthDB: db}
	first, err := decodeResp(resp)
	if err != nil || len(first) == 0 {
		live.put(id, x)
		writeContinue(conn, id, nil)
		return "cont"
	}
	return s.runOAuth(conn, live, id, x, first)
}

// runOAuth checks one client response. A refusal is a challenge carrying the
// JSON; the login fails on the client's answer to it (RFC 7628 §3.2.2).
func (s *Server) runOAuth(conn net.Conn, live *exchanges, id string, x *saslExchange, msg []byte) string {
	parse := parseOAuthBearer
	if x.mech == MechXOAuth2 {
		parse = parseXOAuth2
	}
	refuse := func(status string) func(net.Conn, string) {
		return func(conn net.Conn, id string) {
			x.oauthRefused = true
			live.put(id, x)
			writeContinue(conn, id, oauthFailure(x.mech, status, x.oauthDB))
		}
	}
	user, token, err := parse(msg)
	if errors.Is(err, errOAuthToken) {
		refuse("invalid_token")(conn, id)
		return "fail"
	}
	if err != nil {
		refuse("invalid_request")(conn, id)
		return "fail"
	}
	x.username = user
	return s.authenticatePassword(conn, id, x.service, x.rip, x.session, "", user, token, refuse("invalid_token"))
}

// continueOAuth carries a response into an OAuth login: its first message, or
// the client's acknowledgement of a refusal.
func (s *Server) continueOAuth(conn net.Conn, live *exchanges, id string, x *saslExchange, response []byte) string {
	live.drop(id)
	if x.oauthRefused {
		fmt.Fprintf(conn, "FAIL\t%s\n", id)
		return "fail"
	}
	return s.runOAuth(conn, live, id, x, response)
}
