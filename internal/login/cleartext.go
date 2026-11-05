package login

import (
	"crypto/tls"
	"log/slog"
	"net"

	proxyproto "github.com/pires/go-proxyproto"
)

// cleartextExposedMsg is the IMAP alert sent when the secret is already on the line.
const cleartextExposedMsg = "cleartext authentication not allowed without SSL/TLS, but your client did it anyway. " +
	"If anyone was listening, the password was exposed."

// CleartextRefused reports whether a cleartext mechanism (PLAIN, LOGIN,
// OAUTHBEARER, XOAUTH2, USER/PASS, HTTP Basic and Bearer) must be refused on
// conn: auth_allow_cleartext is off and the connection is not secured.
func CleartextRefused(conn net.Conn, allowCleartext bool, trustedNets []*net.IPNet) bool {
	return !allowCleartext && !connSecured(conn, trustedNets)
}

// connSecured: TLS, a PROXY header the listener accepted (only trusted peers
// get one read), or a socket peer that is local or in trustedNets.
func connSecured(conn net.Conn, trustedNets []*net.IPNet) bool {
	sock := conn
	for sock != nil {
		if _, ok := sock.(*tls.Conn); ok {
			return true
		}
		if pc, ok := sock.(*proxyproto.Conn); ok {
			if pc.ProxyHeader() != nil {
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
	return ipInNets(r.IP.String(), trustedNets)
}

// logCleartextRefused is the log line of every refused cleartext attempt.
func logCleartextRefused(conn net.Conn, sessionID string) {
	ip, _ := hostPort(conn.RemoteAddr())
	slog.Info("login: Login failed: Cleartext authentication disabled", "sid", sessionID, "remote_ip", ip)
}
