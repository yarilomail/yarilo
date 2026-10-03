package main

import (
	"net"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/ftsproto"
	"github.com/yarilomail/yarilo/pkg/mtls"
	"github.com/yarilomail/yarilo/pkg/mtls/mtlstest"
)

// noService answers only the VERSION handshake, which Serve handles itself.
type noService struct{ ftsproto.Service }

// The FTS port is behind internal mTLS and takes the session roles, not others (#2132).
func TestFTSPortIsInternalMTLS(t *testing.T) {
	ca := mtlstest.NewCA(t)
	cfg := &config.Config{}
	cfg.InternalTLS.Enabled = true
	cfg.InternalTLS.Cert, cfg.InternalTLS.Key = ca.Role(t, mtls.RoleFTS)
	cfg.InternalTLS.CA = ca.CAFile

	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := internalListener(cfg, raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go ftsproto.Serve(ln, noService{}) //nolint:errcheck

	for _, tc := range []struct {
		role mtls.Role
		ok   bool
	}{
		{mtls.RoleIMAP, true},
		{mtls.RoleBackendAPI, true},
		{mtls.RoleAuth, false},
	} {
		c, k := ca.Role(t, tc.role)
		cli, err := mtls.ClientConfig(c, k, ca.CAFile, mtlstest.ServerName, -1, 0)
		if err != nil {
			t.Fatal(err)
		}
		r, err := ftsproto.Dial(ln.Addr().String(), cli, 5*time.Second)
		if err == nil {
			r.Close() //nolint:errcheck
		}
		if (err == nil) != tc.ok {
			t.Errorf("%s: dial err %v, want accepted %v", tc.role, err, tc.ok)
		}
	}
	if _, err := ftsproto.Dial(ln.Addr().String(), nil, 2*time.Second); err == nil {
		t.Error("a plain-TCP client completed the VERSION handshake on the mTLS port")
	}
}
