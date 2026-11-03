//go:build linux

package sockaddr

import "golang.org/x/sys/unix"

// PeerSupported reports whether PeerUID works on this OS.
const PeerSupported = true

func peerUID(fd int) (uint32, error) {
	cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0, err
	}
	return cred.Uid, nil
}
