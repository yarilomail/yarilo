package sockaddr

import (
	"errors"
	"fmt"
	"net"
	"os/user"
	"strconv"
)

// ErrPeerUnsupported is returned where the OS gives no peer credentials for a
// Unix socket.
var ErrPeerUnsupported = errors.New("sockaddr: peer credentials are not available on this OS")

// PeerUID returns the uid of the process at the other end of a Unix socket
// connection, as the kernel recorded it at connect.
func PeerUID(c net.Conn) (uint32, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("sockaddr/peer: %T is not a Unix socket connection", c)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("sockaddr/peer: %w", err)
	}
	var uid uint32
	var perr error
	if err := raw.Control(func(fd uintptr) { uid, perr = peerUID(int(fd)) }); err != nil {
		return 0, fmt.Errorf("sockaddr/peer: %w", err)
	}
	if perr != nil {
		return 0, fmt.Errorf("sockaddr/peer: %w", perr)
	}
	return uid, nil
}

// UIDs resolves user names or numeric uids to the set of uids they name.
func UIDs(names []string) (map[uint32]bool, error) {
	out := make(map[uint32]bool, len(names))
	for _, n := range names {
		if id, err := strconv.ParseUint(n, 10, 32); err == nil {
			out[uint32(id)] = true
			continue
		}
		u, err := user.Lookup(n)
		if err != nil {
			return nil, fmt.Errorf("sockaddr/peer: user %q: %w", n, err)
		}
		id, err := strconv.ParseUint(u.Uid, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("sockaddr/peer: user %q has uid %q: %w", n, u.Uid, err)
		}
		out[uint32(id)] = true
	}
	return out, nil
}
