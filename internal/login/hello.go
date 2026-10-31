package login

import (
	"crypto/md5" //nolint:gosec // JA3 is defined as an MD5
	"crypto/tls"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
)

// helloHashes holds each connection's JA3 hash from its TLS hello, until
// handleConn returns.
var helloHashes sync.Map

// recordHello is cfg answering every hello as before and remembering its
// fingerprint for the AUTH line.
func recordHello(cfg *tls.Config) *tls.Config {
	if cfg == nil {
		return nil
	}
	c := cfg.Clone()
	prev := c.GetConfigForClient
	maxVersion := c.MaxVersion
	if maxVersion == 0 {
		maxVersion = tls.VersionTLS13
	}
	c.GetConfigForClient = func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		helloHashes.Store(h.Conn, ja3Hash(h, maxVersion))
		if prev != nil {
			return prev(h)
		}
		return nil, nil
	}
	return c
}

// isGrease is a GREASE value (RFC 8701), left out of the fingerprint.
func isGrease(v uint16) bool { return v&0x0f0f == 0x0a0a && v>>8 == v&0xff }

// ja3Hash is the MD5 of version,ciphers,extensions,groups,point formats, as
// the reference builds it: its version is the server's highest less one.
func ja3Hash(h *tls.ClientHelloInfo, maxVersion uint16) string {
	var b strings.Builder
	b.WriteString(strconv.Itoa(int(maxVersion) - 1))
	b.WriteByte(',')
	for i, c := range h.CipherSuites {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(strconv.Itoa(int(c)))
	}
	b.WriteByte(',')
	join := func(vals []uint16) {
		first := true
		for _, v := range vals {
			if isGrease(v) {
				continue
			}
			if !first {
				b.WriteByte('-')
			}
			first = false
			b.WriteString(strconv.Itoa(int(v)))
		}
	}
	join(h.Extensions)
	b.WriteByte(',')
	curves := make([]uint16, len(h.SupportedCurves))
	for i, c := range h.SupportedCurves {
		curves[i] = uint16(c)
	}
	join(curves)
	b.WriteByte(',')
	for i, p := range h.SupportedPoints {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(strconv.Itoa(int(p)))
	}
	sum := md5.Sum([]byte(b.String())) //nolint:gosec // see above
	return hex.EncodeToString(sum[:])
}
