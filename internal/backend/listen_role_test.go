package backend

import (
	"crypto/tls"
	"net"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// A session backend with no address binds loopback only; the single binary,
// which clients reach directly, binds every interface (#2416).
func TestBindTakesTheRoleHost(t *testing.T) {
	for _, tc := range []struct {
		role string
		want bool // loopback
	}{
		{config.BackendHost, true},
		{config.PublicHost, false},
	} {
		ln, err := bindTCP("imap", &config.ServiceConfig{Enabled: true}, tc.role)
		if err != nil {
			t.Fatal(err)
		}
		ip := ln.Addr().(*net.TCPAddr).IP
		ln.Close()
		if ip.IsLoopback() != tc.want {
			t.Errorf("role %q bound %s, loopback = %v, want %v", tc.role, ip, ip.IsLoopback(), tc.want)
		}
	}
}

func TestBindTLSTakesTheRoleHost(t *testing.T) {
	cfg := &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }}
	ln, err := bindTLS("imaps", &config.ServiceConfig{Enabled: true}, config.BackendHost, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if ip := ln.Addr().(*net.TCPAddr).IP; !ip.IsLoopback() {
		t.Errorf("backend TLS listener bound %s, want loopback", ip)
	}
}
