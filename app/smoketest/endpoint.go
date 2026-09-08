package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"time"
)

// tlsMode is how a protocol's connection is secured. Named per protocol rather
// than assumed from the port: a deployment may serve IMAP plain on 143 behind a
// terminator, and a gate that assumes 993 means TLS tests the wrong thing.
type tlsMode string

const (
	tlsSSL      tlsMode = "ssl"      // TLS from the first byte
	tlsSTARTTLS tlsMode = "starttls" // plain, upgraded in-protocol
	tlsNone     tlsMode = "none"     // plain, never upgraded
)

// parseTLSMode refuses a spelling nothing implements. A typo would otherwise
// fall back to one of the three and report a surface nobody asked for (#1734).
func parseTLSMode(flagName, v string) (tlsMode, error) {
	switch m := tlsMode(strings.ToLower(strings.TrimSpace(v))); m {
	case tlsSSL, tlsSTARTTLS, tlsNone:
		return m, nil
	default:
		return "", fmt.Errorf("-%s %q is none of ssl, starttls, none", flagName, v)
	}
}

// endpoint is one protocol's address as the operator gave it.
type endpoint struct {
	name string // the protocol, for errors
	host string
	port string
	mode tlsMode
	// upgrade performs the in-protocol STARTTLS exchange and returns the
	// connection to wrap. Only consulted for tlsSTARTTLS.
	upgrade func(conn net.Conn, r *bufio.Reader) error
}

func (e endpoint) addr() string { return net.JoinHostPort(e.host, e.port) }

func (e endpoint) tlsConfig() *tls.Config {
	return &tls.Config{ServerName: e.host, InsecureSkipVerify: *flagInsecure} //nolint:gosec
}

// dial opens the connection the mode describes and returns it with the reader
// that has already consumed whatever the handshake read.
func (e endpoint) dial() (net.Conn, *bufio.Reader, error) {
	dialer := &net.Dialer{Timeout: *flagTimeout}
	switch e.mode {
	case tlsSSL:
		conn, err := tls.DialWithDialer(dialer, "tcp", e.addr(), e.tlsConfig())
		if err != nil {
			return nil, nil, fmt.Errorf("%s: connect %s: %w", e.name, e.addr(), err)
		}
		conn.SetDeadline(time.Now().Add(*flagTimeout)) //nolint:errcheck
		return conn, bufio.NewReader(conn), nil
	case tlsNone, tlsSTARTTLS:
		conn, err := dialer.Dial("tcp", e.addr())
		if err != nil {
			return nil, nil, fmt.Errorf("%s: connect %s: %w", e.name, e.addr(), err)
		}
		conn.SetDeadline(time.Now().Add(*flagTimeout)) //nolint:errcheck
		r := bufio.NewReader(conn)
		if e.mode == tlsNone {
			return conn, r, nil
		}
		if e.upgrade == nil {
			conn.Close() //nolint:errcheck
			return nil, nil, fmt.Errorf("%s: starttls asked for, and this check has no upgrade for it", e.name)
		}
		if err := e.upgrade(conn, r); err != nil {
			conn.Close() //nolint:errcheck
			return nil, nil, fmt.Errorf("%s: starttls: %w", e.name, err)
		}
		tconn := tls.Client(conn, e.tlsConfig())
		if err := tconn.Handshake(); err != nil {
			conn.Close() //nolint:errcheck
			return nil, nil, fmt.Errorf("%s: starttls handshake: %w", e.name, err)
		}
		tconn.SetDeadline(time.Now().Add(*flagTimeout)) //nolint:errcheck
		return tconn, bufio.NewReader(tconn), nil
	default:
		return nil, nil, fmt.Errorf("%s: unknown tls mode %q", e.name, e.mode)
	}
}

// lineUpgrade is the STARTTLS exchange of a line protocol whose server answers
// with one line: IMAP and POP3 differ only in the word and the accepted prefix.
func lineUpgrade(command, okPrefix string) func(net.Conn, *bufio.Reader) error {
	return func(conn net.Conn, r *bufio.Reader) error {
		// The greeting first: an upgrade sent into it is answered out of order.
		if _, err := r.ReadString('\n'); err != nil {
			return fmt.Errorf("greeting: %w", err)
		}
		if _, err := fmt.Fprintf(conn, "%s\r\n", command); err != nil {
			return err
		}
		line, err := r.ReadString('\n')
		if err != nil {
			return fmt.Errorf("%s: %w", command, err)
		}
		if !strings.HasPrefix(line, okPrefix) {
			return fmt.Errorf("%s refused: %s", command, strings.TrimRight(line, "\r\n"))
		}
		return nil
	}
}
