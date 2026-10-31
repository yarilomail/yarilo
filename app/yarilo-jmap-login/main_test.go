package main

import (
	"context"
	"net"
	"testing"

	authclient "github.com/yarilomail/yarilo/v2/internal/auth/client"
	"github.com/yarilomail/yarilo/v2/internal/auth/oauth2"
	"github.com/yarilomail/yarilo/v2/internal/auth/protocol"
	"github.com/yarilomail/yarilo/v2/pkg/config"
)

type aliceToken struct{}

func (aliceToken) Validate(_ context.Context, token string) (*oauth2.Claims, error) {
	if token == "good-token" {
		return &oauth2.Claims{Username: "alice"}, nil
	}
	return nil, oauth2.ErrTokenInvalid
}

// The binary wires both credential types, and its Bearer reaches the auth
// service: the step that was missing, so every token was refused (#2228).
func TestTheBinaryWiresBearerToTheAuthService(t *testing.T) {
	db, err := oauth2.NewPassdb(oauth2.PassdbConfig{Validator: aliceToken{}})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go protocol.NewServer([]protocol.Passdb{db}).Serve(ctx, ln) //nolint:errcheck
	t.Cleanup(func() { cancel(); ln.Close() })                  //nolint:errcheck
	auth, err := authclient.Dial(ln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { auth.Close() }) //nolint:errcheck

	cfg := &config.Config{}
	cfg.JMAPLoginService.BackendAddr = "127.0.0.1:1"
	opts := serverOptions(cfg, &config.ServiceConfig{Enabled: true}, ":0", nil, nil, auth, nil)
	if opts.Auth == nil || opts.Tokens == nil {
		t.Fatalf("Basic wired %v, Bearer wired %v; want both", opts.Auth != nil, opts.Tokens != nil)
	}
	if !opts.Tokens.OffersTokens() {
		t.Error("the auth service runs OAUTHBEARER and the proxy would not offer Bearer")
	}
	if user, err := opts.Tokens.AuthenticateToken("good-token", "jmap", "192.0.2.1", "", "s1"); err != nil || user != "alice" {
		t.Errorf("a valid token through the binary's wiring answered %q, %v; want alice", user, err)
	}
}

func TestTheTicketKeyIsCheckedAtStart(t *testing.T) {
	cases := []struct {
		name, key string
		ttl       int
		warn, err bool
	}{
		{"a key", "k3y", 60, false, false},
		{"a placeholder left by a missing Secret", "${JMAP_WEBSOCKET_TICKET_KEY}", 60, false, true},
		{"a placeholder with tickets off", "${JMAP_WEBSOCKET_TICKET_KEY}", 0, false, true},
		{"no key, tickets on", "", 60, true, false},
		{"no key, tickets off", "", 0, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warn, err := checkTicketKey(tc.key, tc.ttl)
			if (warn != "") != tc.warn || (err != nil) != tc.err {
				t.Errorf("checkTicketKey = %q, %v; want warn %v, err %v", warn, err, tc.warn, tc.err)
			}
		})
	}
}
