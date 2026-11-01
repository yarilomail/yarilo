//go:build unix

package dictsrv

import (
	"context"
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/dict"
	"github.com/yarilomail/yarilo/v2/pkg/dict/proxy"
	"github.com/yarilomail/yarilo/v2/pkg/sockaddr"
)

// A session reaches yarilo-dict over a Unix socket with internal TLS
// configured on both sides; neither side wraps the socket in TLS.
func TestADictOverAUnixSocket(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "dict")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "dict.sock")
	addr := "unix:" + path

	real, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	serverTLS := &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }}
	ln, err := Listen(addr, sockaddr.Perm{Mode: 0o640}, serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go New(map[string]dict.Dict{"metadata": real}, nil).Serve(ctx, ln) //nolint:errcheck
	t.Cleanup(func() { cancel(); ln.Close() })                         //nolint:errcheck

	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("socket mode = %v (%v), want 0640", fi.Mode().Perm(), err)
	}

	c := proxy.New(addr, "metadata", &tls.Config{ServerName: "yarilo-internal"})
	t.Cleanup(func() { _ = c.Close() })
	set := &dict.OpSettings{Username: "u1@d.test"}
	tx, err := c.Begin(ctx, set)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Set("priv/one", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatalf("commit over the socket: %v", err)
	}
	vals, found, err := c.Lookup(ctx, set, "priv/one")
	if err != nil || !found || len(vals) != 1 || string(vals[0]) != "first" {
		t.Fatalf("lookup over the socket = %q found=%v err=%v", vals, found, err)
	}
}

func TestADictListenFailureNamesTheAddress(t *testing.T) {
	addr := "unix:/nonexistent-yarilo-dir/dict.sock"
	if _, err := Listen(addr, sockaddr.Perm{Mode: 0o660}, nil); err == nil || !strings.Contains(err.Error(), addr) {
		t.Fatalf("err = %v, want a failure naming %s", err, addr)
	}
}
