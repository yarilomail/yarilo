package login

import (
	"crypto/tls"
	"net"

	proxyproto "github.com/pires/go-proxyproto"
	"github.com/pires/go-proxyproto/tlvparse"
)

// CleartextRefused reports whether a cleartext mechanism (PLAIN, LOGIN,
// OAUTHBEARER, XOAUTH2, USER/PASS, HTTP Basic) must be refused on conn.
// listenerTLS is whether the listener can upgrade the line (STARTTLS/STLS) or
// serves TLS; a listener without TLS keeps cleartext, there is nothing better.
func CleartextRefused(conn net.Conn, listenerTLS, allowCleartext bool) bool {
	return !allowCleartext && listenerTLS && !connSecured(conn)
}

// connSecured: TLS here, a local peer, or a PROXY v2 header whose PP2_TYPE_SSL
// TLV says the client used TLS to the proxy. Trusting the proxy is not enough.
func connSecured(conn net.Conn) bool {
	if conn == nil {
		return false
	}
	if localPeer(conn) {
		return true
	}
	for c := conn; c != nil; {
		switch v := c.(type) {
		case *tls.Conn:
			return true
		case *proxyproto.Conn:
			return proxyClientSSL(v)
		case interface{ NetConn() net.Conn }:
			c = v.NetConn()
		default:
			return false
		}
	}
	return false
}

func localPeer(conn net.Conn) bool {
	r, rok := conn.RemoteAddr().(*net.TCPAddr)
	l, lok := conn.LocalAddr().(*net.TCPAddr)
	return rok && lok && r.IP.Equal(l.IP)
}

func proxyClientSSL(c *proxyproto.Conn) bool {
	h := c.ProxyHeader()
	if h == nil {
		return false
	}
	tlvs, err := h.TLVs()
	if err != nil {
		return false
	}
	ssl, ok := tlvparse.FindSSL(tlvs)
	return ok && ssl.ClientSSL()
}
