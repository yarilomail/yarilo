package login

import (
	"crypto/tls"
	"log/slog"
	"net"

	proxyproto "github.com/pires/go-proxyproto"
	"github.com/pires/go-proxyproto/tlvparse"
)

// cleartextExposedMsg is the IMAP alert sent when the secret is already on the line.
const cleartextExposedMsg = "cleartext authentication not allowed without SSL/TLS, but your client did it anyway. " +
	"If anyone was listening, the password was exposed."

// sslRequiredMsg refuses every authentication before TLS under ssl_mode: required.
const sslRequiredMsg = "Authentication disallowed on non-secure (SSL/TLS) connections."

// CleartextRefused reports whether a cleartext mechanism (PLAIN, LOGIN,
// OAUTHBEARER, XOAUTH2, USER/PASS, HTTP Basic and Bearer) must be refused on
// conn: auth_allow_cleartext is off and the connection is not secured.
func CleartextRefused(conn net.Conn, allowCleartext bool, trustedNets []*net.IPNet) bool {
	return !allowCleartext && !connSecured(conn, trustedNets, false)
}

// connSecured: TLS, a PROXY header the listener accepted (only trusted peers
// get one read), or a socket peer that is local or in trustedNets. Under
// sslRequired a trusted peer does not count and a PROXY header must say the
// client used TLS (PP2_CLIENT_SSL).
func connSecured(conn net.Conn, trustedNets []*net.IPNet, sslRequired bool) bool {
	sock := conn
	for sock != nil {
		if _, ok := sock.(*tls.Conn); ok {
			return true
		}
		if pc, ok := sock.(*proxyproto.Conn); ok {
			if h := pc.ProxyHeader(); h != nil && (!sslRequired || proxyClientSSL(h)) {
				return true
			}
			sock = pc.Raw()
			continue
		}
		inner, ok := sock.(interface{ NetConn() net.Conn })
		if !ok {
			break
		}
		sock = inner.NetConn()
	}
	if sock == nil {
		return false
	}
	r, ok := sock.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return false
	}
	if l, ok := sock.LocalAddr().(*net.TCPAddr); ok && r.IP.Equal(l.IP) {
		return true
	}
	return !sslRequired && ipInNets(r.IP.String(), trustedNets)
}

func proxyClientSSL(h *proxyproto.Header) bool {
	tlvs, err := h.TLVs()
	if err != nil {
		return false
	}
	ssl, ok := tlvparse.FindSSL(tlvs)
	return ok && ssl.ClientSSL()
}

func cleartextRefused(conn net.Conn, opts Options) bool {
	return opts.DisablePlainAuth && !connSecured(conn, opts.XClientNets, opts.SSLRequired)
}

// sslRefused: ssl_mode required and the connection is not secured, so no
// mechanism at all is offered or accepted.
func sslRefused(conn net.Conn, opts Options) bool {
	return opts.SSLRequired && !connSecured(conn, opts.XClientNets, true)
}

// logCleartextRefused is the log line of every refused cleartext attempt.
func logCleartextRefused(conn net.Conn, sessionID string) {
	ip, _ := hostPort(conn.RemoteAddr())
	slog.Info("login: Login failed: Cleartext authentication disabled", "sid", sessionID, "remote_ip", ip)
}

// logSSLRefused is the log line of every attempt refused by ssl_mode required.
func logSSLRefused(conn net.Conn, sessionID string) {
	ip, _ := hostPort(conn.RemoteAddr())
	slog.Info("login: Login failed: SSL required for authentication", "sid", sessionID, "remote_ip", ip)
}
