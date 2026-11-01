// Package sockaddr reads the addresses internal services listen on and dial:
// host:port is TCP, unix:/path is a Unix socket (#2415).
package sockaddr

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const unixPrefix = "unix:"

// Network splits addr into the network and address net.Listen and net.Dial take.
func Network(addr string) (network, address string) {
	if path, ok := strings.CutPrefix(addr, unixPrefix); ok {
		return "unix", path
	}
	return "tcp", addr
}

// IsUnix reports whether addr names a Unix socket.
func IsUnix(addr string) bool {
	network, _ := Network(addr)
	return network == "unix"
}

// Perm is the mode and group a Unix socket gets; TCP ignores it.
type Perm struct {
	Mode os.FileMode
	// Group is a group name; empty keeps the process's group.
	Group string
}

// ParseMode reads an octal mode such as "0660".
func ParseMode(s string) (os.FileMode, error) {
	m, err := strconv.ParseUint(s, 8, 32)
	if err != nil || m > 0o777 {
		return 0, fmt.Errorf("socket mode %q is not an octal mode from 0000 to 0777", s)
	}
	return os.FileMode(m), nil
}

// Listen binds addr. A Unix socket gets perm by chmod after the bind, so the
// umask does not decide it. A socket file nothing answers on is replaced; one a
// live process answers on is left to it and the bind fails.
func Listen(addr string, perm Perm) (net.Listener, error) {
	network, path := Network(addr)
	if network != "unix" {
		return net.Listen(network, path)
	}
	gid := -1
	if perm.Group != "" {
		g, err := user.LookupGroup(perm.Group)
		if err != nil {
			return nil, fmt.Errorf("socket %s: group %q: %w", path, perm.Group, err)
		}
		if gid, err = strconv.Atoi(g.Gid); err != nil {
			return nil, fmt.Errorf("socket %s: group %q has gid %q: %w", path, perm.Group, g.Gid, err)
		}
	}
	ln, err := net.Listen("unix", path)
	if errors.Is(err, syscall.EADDRINUSE) {
		if err := removeStale(path); err != nil {
			return nil, err
		}
		ln, err = net.Listen("unix", path)
	}
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, perm.Mode); err != nil {
		ln.Close() //nolint:errcheck,gosec // the Chmod error is the answer
		return nil, fmt.Errorf("socket %s: %w", path, err)
	}
	if gid >= 0 {
		if err := os.Chown(path, -1, gid); err != nil {
			ln.Close() //nolint:errcheck,gosec // the Chown error is the answer
			return nil, fmt.Errorf("socket %s: group %q: %w", path, perm.Group, err)
		}
	}
	return ln, nil
}

// removeStale removes the socket at path when a dial to it is refused, which
// is what a socket left by a dead process answers.
func removeStale(path string) error {
	c, err := net.DialTimeout("unix", path, time.Second)
	if err == nil {
		c.Close() //nolint:errcheck,gosec // only the answer mattered
		return fmt.Errorf("socket %s: a running process listens on it", path)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("socket %s: in use: %w", path, err)
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode().Type() != os.ModeSocket {
		return fmt.Errorf("socket %s: the path holds something other than a socket", path)
	}
	return os.Remove(path)
}

// ListenTLS is Listen serving tlsCfg over TCP. A Unix socket is served plain:
// its file permissions are the boundary.
func ListenTLS(addr string, perm Perm, tlsCfg *tls.Config) (net.Listener, error) {
	ln, err := Listen(addr, perm)
	if err != nil || tlsCfg == nil {
		return ln, err
	}
	if IsUnix(addr) {
		slog.Info("internal_tls does not apply to a Unix socket; its permissions guard it", "addr", addr)
		return ln, nil
	}
	return tls.NewListener(ln, tlsCfg), nil
}

// noTLS remembers the Unix addresses already logged as dialled without TLS,
// so a pooled client does not repeat the line on every dial.
var noTLS sync.Map

// ForAddr is the TLS config a dial to addr uses: tlsCfg over TCP, none to a
// Unix socket, which is logged once per address when tlsCfg was set.
func ForAddr(addr string, tlsCfg *tls.Config) *tls.Config {
	if tlsCfg == nil || !IsUnix(addr) {
		return tlsCfg
	}
	if _, seen := noTLS.LoadOrStore(addr, true); !seen {
		slog.Info("internal_tls does not apply to a Unix socket; dialling it plain", "addr", addr)
	}
	return nil
}

// DialTLS dials addr through d, with tlsCfg over TCP and plain to a Unix socket.
func DialTLS(ctx context.Context, d *net.Dialer, addr string, tlsCfg *tls.Config) (net.Conn, error) {
	network, address := Network(addr)
	if tlsCfg = ForAddr(addr, tlsCfg); tlsCfg == nil {
		return d.DialContext(ctx, network, address)
	}
	return (&tls.Dialer{NetDialer: d, Config: tlsCfg}).DialContext(ctx, network, address)
}
