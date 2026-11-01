//go:build unix

package client_test

import (
	"context"
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"

	authclient "github.com/yarilomail/yarilo/v2/internal/auth/client"
	"github.com/yarilomail/yarilo/v2/internal/auth/protocol"
	"github.com/yarilomail/yarilo/v2/pkg/sockaddr"
)

// A login proxy reaches yarilo-auth over a Unix socket with internal TLS
// configured on both sides: the socket's permissions guard it, so neither
// side wraps it in TLS (#2415).
func TestAuthOverAUnixSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "auth")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	addr := "unix:" + filepath.Join(dir, "auth.sock")

	srv := protocol.NewServer([]protocol.Passdb{flagged{"no"}})
	serverTLS := &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }}
	ln, err := srv.Listen(addr, sockaddr.Perm{Mode: 0o660}, serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go srv.Serve(ctx, ln)                      //nolint:errcheck
	t.Cleanup(func() { cancel(); ln.Close() }) //nolint:errcheck

	c, err := authclient.Dial(addr, &tls.Config{ServerName: "yarilo-internal"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() }) //nolint:errcheck
	res, err := c.Authenticate("alice", "secret", "imap", "192.0.2.1", "", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Username != "alice" {
		t.Fatalf("authenticated %q over the socket, want alice", res.Username)
	}
}

func TestAnAuthListenFailureNamesTheAddress(t *testing.T) {
	addr := "unix:/nonexistent-yarilo-dir/auth.sock"
	if _, err := protocol.NewServer(nil).Listen(addr, sockaddr.Perm{Mode: 0o660}, nil); err == nil || !strings.Contains(err.Error(), addr) {
		t.Fatalf("client listener: err = %v, want a failure naming %s", err, addr)
	}
	if _, err := protocol.NewMasterServer(nil).Listen(addr, sockaddr.Perm{Mode: 0o660}, nil); err == nil || !strings.Contains(err.Error(), addr) {
		t.Fatalf("master listener: err = %v, want a failure naming %s", err, addr)
	}
}
