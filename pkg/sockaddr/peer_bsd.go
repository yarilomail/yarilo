//go:build darwin || freebsd

package sockaddr

import "golang.org/x/sys/unix"

// PeerSupported reports whether PeerUID works on this OS.
const PeerSupported = true

func peerUID(fd int) (uint32, error) {
	cred, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return 0, err
	}
	return cred.Uid, nil
}
