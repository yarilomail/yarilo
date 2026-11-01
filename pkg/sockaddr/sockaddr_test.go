//go:build unix

package sockaddr

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// sockPath is a socket path short enough for sun_path, which a test's own
// temporary directory can outgrow on macOS.
func sockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sockaddr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

func withUmask(t *testing.T, mask int) {
	t.Helper()
	old := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(old) })
}

func TestNetwork(t *testing.T) {
	for _, tc := range []struct{ addr, network, address string }{
		{"127.0.0.1:9100", "tcp", "127.0.0.1:9100"},
		{":9100", "tcp", ":9100"},
		{"unix:/run/yarilo/auth.sock", "unix", "/run/yarilo/auth.sock"},
	} {
		if n, a := Network(tc.addr); n != tc.network || a != tc.address {
			t.Errorf("Network(%q) = %s %q, want %s %q", tc.addr, n, a, tc.network, tc.address)
		}
	}
}

func TestParseMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want os.FileMode
		ok   bool
	}{
		{"0660", 0o660, true},
		{"600", 0o600, true},
		{"0000", 0, true},
		{"0777", 0o777, true},
		{"1777", 0, false},
		{"0668", 0, false},
		{"rw", 0, false},
		{"", 0, false},
	} {
		got, err := ParseMode(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("ParseMode(%q) = %o, %v; want %o, ok %v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

// Under umask 0 the bind alone leaves the socket 0777; the mode is set after it.
func TestListenSetsTheModeWhateverTheUmask(t *testing.T) {
	withUmask(t, 0)
	path := sockPath(t)
	ln, err := Listen("unix:"+path, Perm{Mode: 0o660})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o660 {
		t.Fatalf("mode = %o, want 660", got)
	}
}

// The kernel checks the socket's mode on connect: a user without write
// permission on it is refused. The process's own user stands in for a
// foreign one by holding no bits itself.
func TestASocketWithoutTheRightsRefusesTheConnection(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root connects whatever the mode")
	}
	withUmask(t, 0o022)
	path := sockPath(t)
	ln, err := Listen("unix:"+path, Perm{Mode: 0o060})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if c, err := new(net.Dialer).DialContext(t.Context(), "unix", path); err == nil {
		c.Close()
		t.Fatal("connected to a socket whose owner bits grant nothing")
	}
}

func TestListenSetsTheGroup(t *testing.T) {
	g, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	if err != nil {
		t.Skip("own group has no name:", err)
	}
	path := sockPath(t)
	ln, err := Listen("unix:"+path, Perm{Mode: 0o660, Group: g.Name})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if gid := st.Sys().(*syscall.Stat_t).Gid; strconv.Itoa(int(gid)) != g.Gid {
		t.Fatalf("gid = %d, want %s", gid, g.Gid)
	}
}

func TestAnUnknownGroupFailsTheListen(t *testing.T) {
	path := sockPath(t)
	if ln, err := Listen("unix:"+path, Perm{Mode: 0o660, Group: "yarilo-no-such-group"}); err == nil {
		ln.Close()
		t.Fatal("listened with a group that does not exist")
	}
}

// A socket file left by a dead process is replaced.
func TestAStaleSocketIsReplaced(t *testing.T) {
	path := sockPath(t)
	old, err := new(net.ListenConfig).Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	old.(*net.UnixListener).SetUnlinkOnClose(false)
	old.Close()

	ln, err := Listen("unix:"+path, Perm{Mode: 0o600})
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	defer ln.Close()
	c, err := new(net.Dialer).DialContext(t.Context(), "unix", path)
	if err != nil {
		t.Fatalf("new socket not accepting: %v", err)
	}
	c.Close()
}

// A second instance does not take the socket from a live one.
func TestALiveSocketIsKept(t *testing.T) {
	path := sockPath(t)
	live, err := Listen("unix:"+path, Perm{Mode: 0o600})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()

	if ln, err := Listen("unix:"+path, Perm{Mode: 0o600}); err == nil {
		ln.Close()
		t.Fatal("a second listener took the socket from a live one")
	} else if !strings.Contains(err.Error(), "running process") {
		t.Fatalf("error %q does not say a process listens", err)
	}
	go func() {
		if c, err := live.Accept(); err == nil {
			c.Close()
		}
	}()
	c, err := new(net.Dialer).DialContext(t.Context(), "unix", path)
	if err != nil {
		t.Fatalf("the live socket stopped accepting: %v", err)
	}
	c.Close()
}

// Something that is not a socket is never removed.
func TestAFileInThePlaceIsKept(t *testing.T) {
	path := sockPath(t)
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ln, err := Listen("unix:"+path, Perm{Mode: 0o600}); err == nil {
		ln.Close()
		t.Fatal("listened over a regular file")
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "keep" {
		t.Fatalf("the file was touched: %q, %v", b, err)
	}
}

// Over a Unix socket TLS is not applied on either side.
func TestTLSIsNotAppliedToAUnixSocket(t *testing.T) {
	path := sockPath(t)
	cfg := &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return nil, nil }}
	ln, err := ListenTLS("unix:"+path, Perm{Mode: 0o600}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Write([]byte("hi"))
			c.Close()
		}
	}()
	c, err := DialTLS(context.Background(), &net.Dialer{}, "unix:"+path, &tls.Config{ServerName: "x"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, ok := c.(*tls.Conn); ok {
		t.Fatal("the dial wrapped a Unix socket in TLS")
	}
	c.SetDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2)
	if _, err := c.Read(buf); err != nil || string(buf) != "hi" {
		t.Fatalf("read %q, %v; want the plain greeting", buf, err)
	}
}

// A group the process may not hand the socket to fails the listen.
func TestAGroupTheProcessIsNotInFailsTheListen(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may give a socket any group")
	}
	g, err := user.LookupGroupId("0")
	if err != nil {
		t.Skip("gid 0 has no name")
	}
	groups, _ := os.Getgroups()
	for _, gid := range append(groups, os.Getgid()) {
		if gid == 0 {
			t.Skip("this process is in gid 0")
		}
	}
	// BSD gives a new file its directory's group, which in /tmp is gid 0
	// already; the directory takes the process's group first.
	path := sockPath(t)
	if err := os.Chown(filepath.Dir(path), -1, os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if ln, err := Listen("unix:"+path, Perm{Mode: 0o660, Group: g.Name}); err == nil {
		ln.Close()
		t.Fatalf("listened with group %s, which this process is not in", g.Name)
	}
}

// A socket nothing answers on but this process may not open is left alone.
func TestASocketThisProcessCannotOpenIsKept(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens whatever the mode")
	}
	path := sockPath(t)
	old, err := new(net.ListenConfig).Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	old.(*net.UnixListener).SetUnlinkOnClose(false)
	old.Close()
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	if ln, err := Listen("unix:"+path, Perm{Mode: 0o600}); err == nil {
		ln.Close()
		t.Fatal("replaced a socket whose state the dial could not tell")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("the socket was removed: %v", err)
	}
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"yarilo-internal"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// Over TCP both sides keep TLS.
func TestTLSIsAppliedOverTCP(t *testing.T) {
	ln, err := ListenTLS("127.0.0.1:0", Perm{}, &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Write([]byte("hi"))
			c.Close()
		}
	}()
	c, err := DialTLS(context.Background(), &net.Dialer{Timeout: 2 * time.Second}, ln.Addr().String(),
		&tls.Config{ServerName: "yarilo-internal", InsecureSkipVerify: true}) //nolint:gosec // a throwaway certificate
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, ok := c.(*tls.Conn); !ok {
		t.Fatal("the dial over TCP is not TLS")
	}
	c.SetDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2)
	if _, err := c.Read(buf); err != nil || string(buf) != "hi" {
		t.Fatalf("read %q, %v; want the greeting over TLS", buf, err)
	}
}

func TestAListenFailureIsReturned(t *testing.T) {
	if ln, err := ListenTLS("unix:/nonexistent-yarilo-dir/s", Perm{Mode: 0o600}, &tls.Config{}); err == nil {
		ln.Close()
		t.Fatal("listened in a directory that does not exist")
	}
}
