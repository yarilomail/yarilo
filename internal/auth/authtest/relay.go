// Package authtest wires a session's auth relay to an in-process auth service,
// so a test that once held a passdb drives the same credential over the wire.
package authtest

import (
	"context"
	"net"
	"testing"

	"github.com/emersion/go-sasl"

	authrelay "github.com/yarilomail/yarilo/internal/auth/client"
	"github.com/yarilomail/yarilo/internal/auth/protocol"
)

// RelayTo serves auth against the given chain and returns a client dialled at
// it. The real server, not a stub of it: the wire is what the cut moved onto.
func RelayTo(t *testing.T, auth protocol.Authenticator) *authrelay.Client {
	t.Helper()
	return relayServing(t, []protocol.Passdb{chainEntry(auth)})
}

// relayServing serves the chain and dials it.
func relayServing(t *testing.T, chain []protocol.Passdb, opts ...protocol.ServerOption) *authrelay.Client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("authtest: listen: %v", err)
	}
	srv := protocol.NewServer(chain, opts...)
	ctx, cancel := context.WithCancel(context.Background())
	go srv.Serve(ctx, ln) //nolint:errcheck
	t.Cleanup(func() {
		cancel()
		ln.Close() //nolint:errcheck
	})

	c, err := authrelay.Dial(ln.Addr().String(), nil)
	if err != nil {
		t.Fatalf("authtest: dial: %v", err)
	}
	t.Cleanup(func() { c.Close() }) //nolint:errcheck
	return c
}

// RelayToMaster also serves master-user impersonation: the masterdb verifies the
// master's own credential and the service switches identity to the target.
func RelayToMaster(t *testing.T, auth protocol.Authenticator, masterUser, masterPass string, targets ...string) *authrelay.Client {
	t.Helper()
	known := make(map[string]bool, len(targets))
	for _, u := range targets {
		known[u] = true
	}
	return relayServing(t, []protocol.Passdb{chainEntry(auth)},
		protocol.WithMasterUsers(true),
		protocol.WithMasterdb([]protocol.Passdb{masterCred{masterUser, masterPass}}),
		protocol.WithUserdb(targetUserdb{known}),
	)
}

// masterCred is a masterdb of one: the real one checks the master's password
// and nothing about the target, which the userdb lookup decides.
type masterCred struct{ user, pass string }

func (m masterCred) Authenticate(req *protocol.Request) (protocol.Result, error) {
	if req.Username != m.user {
		return protocol.ResultNext, nil
	}
	if req.Password != m.pass {
		return protocol.ResultFail, nil
	}
	return protocol.ResultOK, nil
}

// targetUserdb knows the impersonation targets by name: an unknown one is what
// makes the service refuse, as the real userdb does.
type targetUserdb struct{ known map[string]bool }

func (u targetUserdb) Lookup(username string) (*protocol.UserInfo, error) {
	if !u.known[username] {
		return nil, nil
	}
	return &protocol.UserInfo{Username: username}, nil
}

// chainEntry keeps whatever the stub can do: the service announces SCRAM only
// when a driver in its chain can look a verifier up.
func chainEntry(auth protocol.Authenticator) protocol.Passdb {
	s256, ok256 := auth.(protocol.SCRAMSha256Lookup)
	s1, ok1 := auth.(protocol.SCRAMSha1Lookup)
	switch {
	case ok256 && ok1:
		return scramPassdb{passdbOf{auth}, s256, s1}
	case ok256:
		return scram256Passdb{passdbOf{auth}, s256}
	case ok1:
		return scram1Passdb{passdbOf{auth}, s1}
	}
	return passdbOf{auth}
}

type scramPassdb struct {
	passdbOf
	s256 protocol.SCRAMSha256Lookup
	s1   protocol.SCRAMSha1Lookup
}

func (p scramPassdb) LookupSCRAMSha256(u string) (*sasl.ScramCredentials, error) {
	return p.s256.LookupSCRAMSha256(u)
}
func (p scramPassdb) LookupSCRAMSha1(u string) (*sasl.ScramCredentials, error) {
	return p.s1.LookupSCRAMSha1(u)
}

type scram256Passdb struct {
	passdbOf
	s256 protocol.SCRAMSha256Lookup
}

func (p scram256Passdb) LookupSCRAMSha256(u string) (*sasl.ScramCredentials, error) {
	return p.s256.LookupSCRAMSha256(u)
}

type scram1Passdb struct {
	passdbOf
	s1 protocol.SCRAMSha1Lookup
}

func (p scram1Passdb) LookupSCRAMSha1(u string) (*sasl.ScramCredentials, error) {
	return p.s1.LookupSCRAMSha1(u)
}

// passdbOf turns a session-side Authenticator into a chain entry, so existing
// stubs keep deciding and only the path to them changes.
type passdbOf struct{ a protocol.Authenticator }

func (p passdbOf) Authenticate(req *protocol.Request) (protocol.Result, error) {
	res, err := p.a.Authenticate(req.Username, req.Password, req.Service, req.RemoteIP)
	if err != nil {
		return protocol.ResultTempFail, err
	}
	if res == nil || res.Result != protocol.AuthOK {
		return protocol.ResultFail, nil
	}
	for _, tok := range protocol.AuthOKTokens(res) {
		if key, value, ok := cut(tok); ok {
			req.Fields.Set("userdb_"+key, value)
		}
	}
	if res.Username != "" {
		req.Fields.Set("user", res.Username)
	}
	return protocol.ResultOK, nil
}

func cut(tok string) (string, string, bool) {
	for i := 0; i < len(tok); i++ {
		if tok[i] == '=' {
			return tok[:i], tok[i+1:], true
		}
	}
	return "", "", false
}

// PlainOnly adapts a stub that answers only AuthPlain -- the shape submission
// sessions used before the cut -- to the service's chain.
func PlainOnly(a interface{ AuthPlain(string, string) error }) protocol.Authenticator {
	return plainOnly{a}
}

type plainOnly struct {
	a interface{ AuthPlain(string, string) error }
}

func (p plainOnly) Authenticate(username, password, _, _ string) (*protocol.AuthResponse, error) {
	if err := p.a.AuthPlain(username, password); err != nil {
		return &protocol.AuthResponse{Result: protocol.AuthFail}, nil
	}
	return &protocol.AuthResponse{Result: protocol.AuthOK, Username: username}, nil
}
