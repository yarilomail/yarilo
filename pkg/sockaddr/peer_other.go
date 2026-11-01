//go:build !linux && !darwin && !freebsd

package sockaddr

// PeerSupported reports whether PeerUID works on this OS.
const PeerSupported = false

func peerUID(int) (uint32, error) {
	return 0, ErrPeerUnsupported
}
