package director

import (
	keyed "crypto/hmac"
	"crypto/sha256"
	"strconv"
)

// peerProof binds a ring connection to the ring secret: the acceptor's nonce
// for this connection and the dialer's ME, labelled so a join proof is not one.
func peerProof(secret []byte, nonce string, dialer Member) []byte {
	m := keyed.New(sha256.New, secret)
	m.Write([]byte("PEER\t" + nonce + "\t" + dialer.IP + "\t" + strconv.Itoa(dialer.Port)))
	return m.Sum(nil)
}
