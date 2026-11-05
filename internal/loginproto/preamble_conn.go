package loginproto

import (
	"bufio"
	"net"
)

// NewPreambleConn returns c as a session sees it after a verified preamble line
// for username: what a listener hands over, for session tests outside this package.
func NewPreambleConn(c net.Conn, username, home, sessionID string) *PreambleConn {
	return &PreambleConn{Conn: c, Username: username, Home: home, SessionID: sessionID,
		realAddr: c.RemoteAddr(), br: bufio.NewReader(c)}
}
