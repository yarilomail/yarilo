package submission

import (
	"net"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/auth/authtest"

	"github.com/emersion/go-sasl"
	goSmtp "github.com/emersion/go-smtp"

	"github.com/yarilomail/yarilo/pkg/config"
)

// oauthBearerAuth implements the submission Authenticator surface
// (AuthPlain) with token-as-password semantics — that's how the
// OAUTHBEARER session-side callback delivers the bearer token.
type oauthBearerAuth struct {
	user, token string
}

func (a oauthBearerAuth) AuthPlain(u, p string) error {
	if u == a.user && p == a.token {
		return nil
	}
	return goSmtp.ErrAuthFailed
}

func buildOAuthBearerServer(t *testing.T, auth Authenticator, enabled bool) (string, func()) {
	t.Helper()
	opts := Options{
		Config: config.SubmissionProtocolConfig{
			Hostname:   "mx.example.com",
			MaxMsgSize: 1 << 20,
		},
		AuthRelay:     authtest.RelayTo(t, authtest.PlainOnly(auth)),
		OAuth2Enabled: enabled,
	}
	srv := New(opts)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln, nil) }()
	return ln.Addr().String(), func() { ln.Close() }
}

// TestSubmission_AuthOAuthBearer_Success — real go-smtp client
// posts an AUTH OAUTHBEARER, server validates, MAIL FROM works.
func TestSubmission_AuthOAuthBearer_Success(t *testing.T) {
	addr, cleanup := buildOAuthBearerServer(t,
		oauthBearerAuth{user: "alice@example.com", token: "valid-token"},
		true,
	)
	defer cleanup()

	c := dialSMTP(t, addr)
	defer c.Close()

	saslClient := sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
		Username: "alice@example.com",
		Token:    "valid-token",
	})
	if err := c.Auth(saslClient); err != nil {
		t.Fatalf("AUTH OAUTHBEARER: %v", err)
	}
	if err := c.Mail("alice@example.com", nil); err != nil {
		t.Fatalf("MAIL FROM after AUTH: %v", err)
	}
}

// TestSubmission_AuthOAuthBearer_InvalidToken — wrong token →
// JSON failure surfaces as auth error.
func TestSubmission_AuthOAuthBearer_InvalidToken(t *testing.T) {
	addr, cleanup := buildOAuthBearerServer(t,
		oauthBearerAuth{user: "alice@example.com", token: "valid-token"},
		true,
	)
	defer cleanup()

	c := dialSMTP(t, addr)
	defer c.Close()

	saslClient := sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
		Username: "alice@example.com",
		Token:    "WRONG",
	})
	if err := c.Auth(saslClient); err == nil {
		t.Errorf("AUTH OAUTHBEARER bad token: expected error, got nil")
	}
}

// TestSubmission_AuthOAuthBearer_DisabledNotAdvertised — when
// OAuth2Enabled is false, the EHLO AUTH extension MUST NOT
// advertise OAUTHBEARER.
func TestSubmission_AuthOAuthBearer_DisabledNotAdvertised(t *testing.T) {
	addr, cleanup := buildOAuthBearerServer(t,
		oauthBearerAuth{user: "alice@example.com", token: "valid-token"},
		false,
	)
	defer cleanup()

	c := dialSMTP(t, addr)
	defer c.Close()

	ok, params := c.Extension("AUTH")
	if !ok {
		t.Fatalf("AUTH extension missing")
	}
	if strings.Contains(params, "OAUTHBEARER") {
		t.Errorf("OAUTHBEARER advertised while disabled: %q", params)
	}
}

// TestSubmission_AuthOAuthBearer_EnabledAdvertised — when on,
// EHLO carries it.
func TestSubmission_AuthOAuthBearer_EnabledAdvertised(t *testing.T) {
	addr, cleanup := buildOAuthBearerServer(t,
		oauthBearerAuth{user: "alice@example.com", token: "valid-token"},
		true,
	)
	defer cleanup()

	c := dialSMTP(t, addr)
	defer c.Close()

	ok, params := c.Extension("AUTH")
	if !ok {
		t.Fatalf("AUTH extension missing")
	}
	if !strings.Contains(params, "OAUTHBEARER") {
		t.Errorf("OAUTHBEARER not advertised when enabled: %q", params)
	}
}

// recordingBearer answers one token and says whether it was ever asked.
type recordingBearer struct {
	user, token string
	asked       *string
}

func (a recordingBearer) AuthPlain(u, p string) error {
	*a.asked = u + "/" + p
	if u == a.user && p == a.token {
		return nil
	}
	return goSmtp.ErrAuthFailed
}

// A bearer presented on 587 is verified in the auth service, not beside the
// session: after the cut there is nowhere else it could be checked (#1733).
func TestABearerOn587IsVerifiedInTheService(t *testing.T) {
	var asked string
	addr, stop := buildOAuthBearerServer(t, recordingBearer{"alice@example.com", "tok-587", &asked}, true)
	defer stop()

	c, err := goSmtp.Dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close() //nolint:errcheck
	if err := c.Auth(sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
		Username: "alice@example.com",
		Token:    "tok-587",
	})); err != nil {
		t.Fatalf("bearer refused: %v", err)
	}
	if asked != "alice@example.com/tok-587" {
		t.Errorf("the service was asked %q, want the token this client sent", asked)
	}
}
