//go:build unix

package warden_test

import (
	"context"
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/warden"
	"github.com/yarilomail/yarilo/v2/pkg/sockaddr"
)

// A login proxy reaches yarilo-warden over a Unix socket with internal TLS
// configured on both sides; neither side wraps the socket in TLS (#2415).
func TestWardenOverAUnixSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "warden")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	addr := "unix:" + filepath.Join(dir, "warden.sock")

	srv := warden.NewServer(10)
	serverTLS := &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }}
	ln, err := srv.Listen(addr, sockaddr.Perm{Mode: 0o660}, serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go srv.Serve(ctx, ln)                      //nolint:errcheck
	t.Cleanup(func() { cancel(); ln.Close() }) //nolint:errcheck

	c, err := warden.Dial(addr, &tls.Config{ServerName: "yarilo-internal"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() }) //nolint:errcheck
	if err := c.Connect("s1", "alice", "192.0.2.1", "imap"); err != nil {
		t.Fatalf("connect over the socket: %v", err)
	}
}

func TestAWardenListenFailureNamesTheAddress(t *testing.T) {
	addr := "unix:/nonexistent-yarilo-dir/warden.sock"
	if _, err := warden.NewServer(1).Listen(addr, sockaddr.Perm{Mode: 0o660}, nil); err == nil || !strings.Contains(err.Error(), addr) {
		t.Fatalf("err = %v, want a failure naming %s", err, addr)
	}
}
