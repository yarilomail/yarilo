package login

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"strings"

	authclient "github.com/yarilomail/yarilo/internal/auth/client"
)

// relayDialer hands the proxy the auth-service client. It is the Server's own
// lazily dialled one, so a proxy holds a single connection per process.
type relayDialer func() (*authclient.Client, error)

// relayContext is what a relayed exchange needs from the connection it serves:
// where to reach the service, and the session id the token is issued for.
type relayContext struct {
	dial      relayDialer
	sessionID string
}

// scramMechanisms are what the service announced. The proxy runs none of them,
// so naming one it cannot relay is a promise it cannot keep.
func scramMechanisms(rc relayContext, conn net.Conn) []string {
	dial := rc.dial
	if dial == nil {
		return nil
	}
	cl, err := dial()
	if err != nil {
		return nil
	}
	bound := channelBinding(conn) != nil
	var out []string
	for _, mech := range cl.Mechanisms() {
		if !strings.HasPrefix(mech, "SCRAM-") {
			continue
		}
		if strings.HasSuffix(mech, "-PLUS") && !bound {
			continue
		}
		out = append(out, mech)
	}
	return out
}

// channelBinding is the RFC 9266 exporter output of the client's TLS
// connection. The proxy terminates TLS, so these bytes exist only here.
func channelBinding(conn net.Conn) []byte {
	tc, ok := conn.(*tls.Conn)
	if !ok {
		return nil
	}
	state := tc.ConnectionState()
	if state.Version < tls.VersionTLS13 {
		return nil
	}
	material, err := state.ExportKeyingMaterial("EXPORTER-Channel-Binding", nil, 32)
	if err != nil {
		return nil
	}
	return material
}

// saslRelayOutcome is what a relayed exchange produced: an authenticated
// identity, or a refusal the caller reports in its own protocol's words.
type saslRelayOutcome struct {
	username string
	result   *authclient.AuthResult
}

// runRelayedSASL drives one exchange. writeChallenge and readResponse are the
// protocol's own spellings of sending and reading the SASL bytes.
func runRelayedSASL(
	rc relayContext,
	conn net.Conn,
	mech, service, clientIP string,
	initial []byte,
	writeChallenge func([]byte) error,
	readResponse func() ([]byte, error),
) (*saslRelayOutcome, error) {
	if rc.dial == nil {
		return nil, fmt.Errorf("login: no auth relay configured")
	}
	cl, err := rc.dial()
	if err != nil {
		return nil, fmt.Errorf("login: auth relay unavailable: %w", err)
	}
	var cb []byte
	if strings.HasSuffix(mech, "-PLUS") {
		if cb = channelBinding(conn); cb == nil {
			return nil, fmt.Errorf("login: %s needs a channel binding this connection has none of", mech)
		}
	}
	srv := authclient.NewRelayServer(cl, mech, service, clientIP, rc.sessionID, cb)
	// An exchange the client abandons frees the service's half at once, rather
	// than waiting out its deadline there (#1733).
	defer srv.Cancel()

	response := initial
	for {
		challenge, done, nerr := srv.Next(response)
		if nerr != nil {
			return nil, nerr
		}
		if done {
			// RFC 5802 §5: a login whose v= never arrives is one a real client
			// refuses after we accepted it. Deliver it, await the empty ack.
			if len(challenge) > 0 {
				if werr := writeChallenge(challenge); werr != nil {
					return nil, werr
				}
				if _, rerr := readResponse(); rerr != nil {
					return nil, rerr
				}
			}
			return &saslRelayOutcome{username: srv.Result.Username, result: srv.Result}, nil
		}
		if werr := writeChallenge(challenge); werr != nil {
			return nil, werr
		}
		response, err = readResponse()
		if err != nil {
			return nil, err
		}
	}
}

// saslIO is how both IMAP and POP3 spell a challenge and a response: "+ " and
// base64, with "*" for a client that walks away.
func saslIO(conn net.Conn, rd *bufio.Reader) (func([]byte) error, func() ([]byte, error)) {
	write := func(challenge []byte) error {
		_, err := fmt.Fprintf(conn, "+ %s\r\n", base64.StdEncoding.EncodeToString(challenge))
		return err
	}
	read := func() ([]byte, error) {
		line, err := rd.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "*" {
			return nil, fmt.Errorf("login: the client cancelled the exchange")
		}
		return base64.StdEncoding.DecodeString(line)
	}
	return write, read
}
