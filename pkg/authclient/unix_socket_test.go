//go:build unix

package authclient_test

import (
	"context"
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/auth/protocol"
	"github.com/yarilomail/yarilo/v2/pkg/authclient"
	"github.com/yarilomail/yarilo/v2/pkg/sockaddr"
)

// A backend reaches the master listener over a Unix socket with internal TLS
// configured on both sides; neither side wraps the socket in TLS (#2415).
func TestMasterOverAUnixSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "master")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	addr := "unix:" + filepath.Join(dir, "master.sock")

	srv := protocol.NewMasterServer(&stubIteratorUserdb{users: map[string]*protocol.UserInfo{"alice": {Home: "/home/alice"}}})
	serverTLS := &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }}
	ln, err := srv.Listen(addr, sockaddr.Perm{Mode: 0o660}, serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go srv.Serve(ctx, ln)                      //nolint:errcheck
	t.Cleanup(func() { cancel(); ln.Close() }) //nolint:errcheck

	// A server that wrapped the socket in TLS would leave the greeting read
	// waiting, so the dial runs under a deadline of its own.
	type dialed struct {
		c   *authclient.Client
		err error
	}
	done := make(chan dialed, 1)
	go func() {
		c, err := authclient.DialContext(context.Background(), addr, &tls.Config{ServerName: "yarilo-internal"})
		done <- dialed{c, err}
	}()
	var c *authclient.Client
	select {
	case d := <-done:
		if d.err != nil {
			t.Fatal(d.err)
		}
		c = d.c
	case <-time.After(5 * time.Second):
		t.Fatal("no greeting over the socket within 5s")
	}
	t.Cleanup(func() { c.Close() }) //nolint:errcheck
	ui, err := c.Userdb(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if ui == nil || ui.Home != "/home/alice" {
		t.Fatalf("userdb over the socket = %+v, want alice's home", ui)
	}
}
