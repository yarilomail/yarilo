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

// scramMechanisms are the SCRAM mechanisms the service announced. The proxy
// advertises these rather than a fixed list: it runs none of them itself, so
// naming one the service cannot serve would be a promise it cannot keep.
func scramMechanisms(dial relayDialer, conn net.Conn) []string {
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
	// final is the server's last message, handed to the client with the
	// success so a client verifying the server signature still gets it.
	final []byte
}

// runRelayedSASL drives one exchange between the mail client and the service.
// readResponse and writeChallenge are the protocol's own spellings of "send
// the client these bytes" and "read the client's answer".
func runRelayedSASL(
	dial relayDialer,
	conn net.Conn,
	mech, service, clientIP, sessionID string,
	initial []byte,
	writeChallenge func([]byte) error,
	readResponse func() ([]byte, error),
) (*saslRelayOutcome, error) {
	if dial == nil {
		return nil, fmt.Errorf("login: no auth relay configured")
	}
	cl, err := dial()
	if err != nil {
		return nil, fmt.Errorf("login: auth relay unavailable: %w", err)
	}
	var cb []byte
	if strings.HasSuffix(mech, "-PLUS") {
		if cb = channelBinding(conn); cb == nil {
			return nil, fmt.Errorf("login: %s needs a channel binding this connection has none of", mech)
		}
	}
	srv := authclient.NewRelayServer(cl, mech, service, clientIP, sessionID, cb)
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
			return &saslRelayOutcome{
				username: srv.Result.Username,
				result:   srv.Result,
				final:    challenge,
			}, nil
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
