package main

import (
	"crypto/tls"
	"net"
	"testing"
	"time"

	submsvr "github.com/yarilomail/yarilo/internal/submission"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/mtls"
	"github.com/yarilomail/yarilo/pkg/mtls/mtlstest"
)

// The login pod dials this backend with internal mTLS; the listener main
// builds completes it for submission-login only (#2133, #2132).
func TestSubmissionBackendTerminatesInternalTLS(t *testing.T) {
	ca := mtlstest.NewCA(t)
	cfg := &config.Config{}
	cfg.InternalTLS.Enabled = true
	cfg.InternalTLS.Cert, cfg.InternalTLS.Key = ca.Role(t, mtls.RoleSubmission)
	cfg.InternalTLS.CA = ca.CAFile

	srv, err := newServer(cfg, submsvr.Options{AuthAddr: "127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = srv.Serve(ln, nil) }()

	for _, tc := range []struct {
		role mtls.Role
		ok   bool
	}{
		{mtls.RoleSubmissionLogin, true},
		{mtls.RoleIMAPLogin, false},
	} {
		c, k := ca.Role(t, tc.role)
		clientCfg, err := mtls.ClientConfig(c, k, ca.CAFile, mtlstest.ServerName, -1, 0)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", ln.Addr().String(), clientCfg)
		if err == nil {
			// TLS 1.3 reports a refused client certificate on the first read.
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			_, err = conn.Read(make([]byte, 1))
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				err = nil
			}
			conn.Close()
		}
		if (err == nil) != tc.ok {
			t.Errorf("%s: handshake err %v, want accepted %v", tc.role, err, tc.ok)
		}
	}
}
