package director

import (
	"bufio"
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/mtls"
	"github.com/yarilomail/yarilo/pkg/mtls/mtlstest"
)

// A command outside the peer's role closes the connection; its own commands
// and PING keep it (#2132).
func TestDirectorPortGatesCommandsByRole(t *testing.T) {
	ca := mtlstest.NewCA(t)
	srvCert, srvKey := ca.Role(t, mtls.RoleDirector)
	srvCfg, err := mtls.ServerConfig(srvCert, srvKey, ca.CAFile, mtls.ListenerDirector)
	if err != nil {
		t.Fatal(err)
	}
	addr := startTLSDirector(t, srvCfg)

	for _, tc := range []struct {
		name   string
		role   mtls.Role
		cmd    string
		closed bool
	}{
		{"login may not register a backend", mtls.RoleIMAPLogin, "BACKEND-UP\t10.0.0.9\t993\t\t1\n", true},
		{"backend-reg may not open a session", mtls.RoleBackendReg, "SESSION-OPEN\t1\tu@d\t10.0.0.9\n", true},
		{"backend-reg may register", mtls.RoleBackendReg, "BACKEND-UP\t10.0.0.9\t993\t\t1\n", false},
		{"login may look up", mtls.RoleIMAPLogin, "LOOKUP\t1\tu@d\t\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, sc := dialRole(t, ca, addr, tc.role)
			readHandshake(t, sc)
			sendHandshake(t, conn)
			if _, err := conn.Write([]byte(tc.cmd + "PING\n")); err != nil {
				t.Fatal(err)
			}
			sawPong := false
			for sc.Scan() {
				if sc.Text() == "PONG" {
					sawPong = true
					break
				}
			}
			if sawPong == tc.closed {
				t.Fatalf("PONG after %q from %s: %v, want %v", tc.cmd, tc.role, sawPong, !tc.closed)
			}
		})
	}
}

// Joining the ring is the director's alone: anyone else is closed before the join reply.
func TestDirectorJoinNeedsTheDirectorRole(t *testing.T) {
	ca := mtlstest.NewCA(t)
	srvCert, srvKey := ca.Role(t, mtls.RoleDirector)
	srvCfg, err := mtls.ServerConfig(srvCert, srvKey, ca.CAFile, mtls.ListenerDirector)
	if err != nil {
		t.Fatal(err)
	}
	addr := startTLSDirector(t, srvCfg)
	for _, tc := range []struct {
		role  mtls.Role
		reply bool
	}{
		{mtls.RoleDirector, true},
		{mtls.RoleIMAPLogin, false},
		{mtls.RoleBackendReg, false},
	} {
		conn, sc := dialRole(t, ca, addr, tc.role)
		readHandshake(t, sc)
		if _, err := conn.Write([]byte("DIRECTOR-JOIN\t10.0.0.7\t9090\n")); err != nil {
			t.Fatal(err)
		}
		if got := sc.Scan(); got != tc.reply {
			t.Errorf("DIRECTOR-JOIN from %s: reply %v (%q), want %v", tc.role, got, sc.Text(), tc.reply)
		}
	}
}

func startTLSDirector(t *testing.T, cfg *tls.Config) string {
	t.Helper()
	srv := NewWithOptions(Options{PingInterval: 24 * time.Hour})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); ln.Close() })
	go func() { _ = srv.listenOn(ctx, tls.NewListener(ln, cfg)) }()
	return ln.Addr().String()
}

func dialRole(t *testing.T, ca *mtlstest.CA, addr string, r mtls.Role) (net.Conn, *bufio.Scanner) {
	t.Helper()
	c, k := ca.Role(t, r)
	cliCfg, err := mtls.ClientConfig(c, k, ca.CAFile, mtlstest.ServerName, -1, 0)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := tls.Dial("tcp", addr, cliCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	return conn, bufio.NewScanner(conn)
}
