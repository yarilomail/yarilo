package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/pbkdf2"
)

// Row: SCRAM through the login proxy. The proxy runs no mechanism itself — it
// relays to yarilo-auth — so this is the check that the relay is wired where
// clients actually connect (#1733).
func checkSCRAMThroughProxy(user, pass string, plus bool) error {
	mech := "SCRAM-SHA-256"
	if plus {
		mech = "SCRAM-SHA-256-PLUS"
	}
	c, err := imapDial()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer c.close()

	caps, err := c.cmd("CAPABILITY")
	if err != nil {
		return fmt.Errorf("capability: %w", err)
	}
	if !anyLineHas(caps, "AUTH="+mech, "") {
		return fmt.Errorf("the proxy does not offer AUTH=%s: %s", mech, strings.Join(caps, " | "))
	}

	var binding []byte
	if plus {
		tc, ok := c.conn.(*tls.Conn)
		if !ok {
			return fmt.Errorf("%s needs TLS, and this connection is plain", mech)
		}
		binding, err = exporterBinding(tc)
		if err != nil {
			return err
		}
	}
	return c.scramLogin(mech, user, pass, binding)
}

// exporterBinding is the RFC 9266 channel binding of this connection, which is
// the same material the proxy holds as the TLS terminator.
func exporterBinding(conn *tls.Conn) ([]byte, error) {
	state := conn.ConnectionState()
	if state.Version < tls.VersionTLS13 {
		return nil, fmt.Errorf("channel binding needs TLS 1.3, this connection is 0x%x", state.Version)
	}
	material, err := state.ExportKeyingMaterial("EXPORTER-Channel-Binding", nil, 32)
	if err != nil {
		return nil, fmt.Errorf("export keying material: %w", err)
	}
	return material, nil
}

// scramLogin runs the exchange and requires the tagged OK. The client side is
// written out rather than taken from a library, so what travels is visible.
func (c *imapClient) scramLogin(mech, user, pass string, binding []byte) error {
	gs2 := "n,,"
	if binding != nil {
		gs2 = "p=tls-exporter,,"
	}
	nonce := "yariloSmokeNonce"
	firstBare := fmt.Sprintf("n=%s,r=%s", user, nonce)

	// Written on the wire directly: the exchange is interactive, and the
	// tagged-command helper would consume the continuation as an untagged line.
	challenge, err := c.saslStart(fmt.Sprintf("AUTHENTICATE %s %s", mech,
		base64.StdEncoding.EncodeToString([]byte(gs2+firstBare))))
	if err != nil {
		return fmt.Errorf("%s: %w", mech, err)
	}
	serverFirst, err := base64.StdEncoding.DecodeString(challenge)
	if err != nil {
		return fmt.Errorf("%s: challenge is not base64: %w", mech, err)
	}

	final, err := scramClientFinal(string(serverFirst), gs2, binding, firstBare, pass, nonce)
	if err != nil {
		return fmt.Errorf("%s: %w", mech, err)
	}
	if err := c.saslFinish(base64.StdEncoding.EncodeToString([]byte(final))); err != nil {
		return fmt.Errorf("%s: %w", mech, err)
	}
	return nil
}

// scramClientFinal computes the client-final message, proof and all.
func scramClientFinal(serverFirst, gs2 string, binding []byte, firstBare, pass, nonce string) (string, error) {
	var combined, saltB64 string
	var iter int
	for _, attr := range strings.Split(serverFirst, ",") {
		if len(attr) < 2 {
			continue
		}
		switch attr[0] {
		case 'r':
			combined = attr[2:]
		case 's':
			saltB64 = attr[2:]
		case 'i':
			fmt.Sscanf(attr[2:], "%d", &iter) //nolint:errcheck
		}
	}
	if combined == "" || !strings.HasPrefix(combined, nonce) {
		return "", fmt.Errorf("the server nonce %q does not extend the client's", combined)
	}
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		return "", fmt.Errorf("salt is not base64: %w", err)
	}
	cb := append([]byte(gs2), binding...)
	withoutProof := fmt.Sprintf("c=%s,r=%s", base64.StdEncoding.EncodeToString(cb), combined)
	authMessage := firstBare + "," + serverFirst + "," + withoutProof

	salted := pbkdf2.Key([]byte(pass), salt, iter, sha256.Size, sha256.New)
	clientKey := hmacSHA256(salted, []byte("Client Key"))
	stored := sha256.Sum256(clientKey)
	sig := hmacSHA256(stored[:], []byte(authMessage))
	proof := make([]byte, len(clientKey))
	for i := range proof {
		proof[i] = clientKey[i] ^ sig[i]
	}
	return fmt.Sprintf("%s,p=%s", withoutProof, base64.StdEncoding.EncodeToString(proof)), nil
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data) //nolint:errcheck
	return m.Sum(nil)
}

// saslStart sends the AUTHENTICATE command and returns the base64 challenge.
func (c *imapClient) saslStart(command string) (string, error) {
	c.seq++
	c.tag = fmt.Sprintf("S%04d", c.seq)
	c.conn.SetDeadline(time.Now().Add(*flagIMAPReadTimeout)) //nolint:errcheck
	fmt.Fprintf(c.conn, "%s %s\r\n", c.tag, command)
	line, err := c.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "+ ") {
		return "", fmt.Errorf("expected a challenge, got %q", line)
	}
	return strings.TrimSpace(line[2:]), nil
}

// saslFinish sends the client's last message and requires the tagged OK.
func (c *imapClient) saslFinish(response string) error {
	fmt.Fprintf(c.conn, "%s\r\n", response)
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, c.tag+" OK") {
			return nil
		}
		if strings.HasPrefix(line, c.tag+" ") {
			return fmt.Errorf("%s", line)
		}
	}
}
