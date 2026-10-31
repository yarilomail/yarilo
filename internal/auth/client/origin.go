package client

import (
	"errors"
	"strconv"
	"strings"

	"github.com/yarilomail/yarilo/v2/internal/auth/protocol"
)

// Option is something the login process knows about the mail client's
// connection, carried on the AUTH line for the policy server and the penalty.
type Option func(*origin)

type origin struct {
	secured   string
	clientID  string
	noPenalty bool
	addrs     Addresses
	localName string
	ja3Hash   string
}

// Addresses are the client connection's ports, and the TCP peer's own
// address and ports where a proxy or a forwarder stands between.
type Addresses struct {
	LocalPort, RemotePort         int
	RealLocalIP, RealRemoteIP     string
	RealLocalPort, RealRemotePort int
}

// Conn gives the connection's ports and real addresses.
func Conn(a Addresses) Option { return func(o *origin) { o.addrs = a } }

// LocalName is the server name the client asked for in TLS.
func LocalName(name string) Option { return func(o *origin) { o.localName = name } }

// JA3Hash is the MD5 of the client's TLS hello fingerprint.
func JA3Hash(h string) Option { return func(o *origin) { o.ja3Hash = h } }

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
	a := o.addrs
	for _, f := range []struct {
		key string
		n   int
	}{{"lport", a.LocalPort}, {"rport", a.RemotePort}, {"real_lport", a.RealLocalPort}, {"real_rport", a.RealRemotePort}} {
		if f.n > 0 {
			sb.WriteString("\t" + f.key + "=" + strconv.Itoa(f.n))
		}
	}
	for _, f := range []struct{ key, v string }{
		{"real_lip", a.RealLocalIP}, {"real_rip", a.RealRemoteIP},
		{"local_name", o.localName}, {"ssl_ja3_hash", o.ja3Hash}, {"client_id", o.clientID},
	} {
		if f.v != "" {
			sb.WriteString("\t" + f.key + "=" + protocol.TabEscape(f.v))
		}
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
