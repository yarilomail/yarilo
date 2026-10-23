package client

import (
	"errors"
	"strings"

	"github.com/yarilomail/yarilo/internal/auth/protocol"
)

// Option is something the login process knows about the mail client's
// connection, carried on the AUTH line for the policy server and the penalty.
type Option func(*origin)

type origin struct {
	secured   string
	clientID  string
	noPenalty bool
}

// TLS says the mail client's connection is TLS.
func TLS() Option { return func(o *origin) { o.secured = "tls" } }

// ClientID is what the client said about itself, the IMAP ID pairs.
func ClientID(id string) Option { return func(o *origin) { o.clientID = id } }

// NoPenalty is a client the login process trusts: no tarpit and no penalty.
func NoPenalty() Option { return func(o *origin) { o.noPenalty = true } }

func writeOrigin(sb *strings.Builder, opts []Option) {
	var o origin
	for _, opt := range opts {
		opt(&o)
	}
	if o.secured != "" {
		sb.WriteString("\tsecured=")
		sb.WriteString(o.secured)
	}
	if o.noPenalty {
		sb.WriteString("\tno-penalty")
	}
	if o.clientID != "" {
		sb.WriteString("\tclient_id=")
		sb.WriteString(protocol.TabEscape(o.clientID))
	}
}

// RefusedError is a refusal the service gave a reason for, meant for the mail
// client; it is an ErrAuthFailed.
type RefusedError struct{ Reason string }

func (e *RefusedError) Error() string { return "auth/client: refused: " + e.Reason }

func (e *RefusedError) Is(target error) bool { return target == ErrAuthFailed }

// RefusalReason is the reason to show the mail client for err, if it has one.
func RefusalReason(err error) string {
	var r *RefusedError
	if errors.As(err, &r) {
		return r.Reason
	}
	return ""
}
