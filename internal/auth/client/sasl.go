package client

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// SASLStep is one round of a relayed exchange: either a challenge to hand back
// to the mail client, or the verdict.
type SASLStep struct {
	// Challenge is the server's bytes when the exchange continues.
	Challenge []byte
	// Done is set when Result carries the verdict.
	Done bool
	// Result is the authentication outcome once Done.
	Result *AuthResult
	// Final carries the server's last message alongside a successful verdict,
	// which a client verifying the server signature still needs.
	Final []byte
}

// SASLExchange is a conversation in flight. The session holds it and carries
// bytes; the mechanism runs in the auth service (#1733).
type SASLExchange struct {
	c  *Client
	id string
}

// Mechanisms are the SASL mechanisms the service announced at handshake. A
// session advertises these, not what it could run itself.
func (c *Client) Mechanisms() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.mechs))
	copy(out, c.mechs)
	return out
}

// BeginSASL starts a relayed exchange. cbind is the channel binding the session
// holds: it terminates TLS, so those bytes exist nowhere else.
func (c *Client) BeginSASL(mech, service, remoteIP, sessionID string, cbind, initial []byte) (*SASLExchange, *SASLStep, error) {
	id := c.nextID()
	var sb strings.Builder
	sb.WriteString("AUTH\t")
	sb.WriteString(id)
	sb.WriteString("\t")
	sb.WriteString(mech)
	sb.WriteString("\tresp=")
	sb.WriteString(base64.StdEncoding.EncodeToString(initial))
	if service != "" {
		sb.WriteString("\tservice=")
		sb.WriteString(service)
	}
	if remoteIP != "" {
		sb.WriteString("\trip=")
		sb.WriteString(remoteIP)
	}
	if sessionID != "" {
		sb.WriteString("\tsession=")
		sb.WriteString(sessionID)
	}
	if len(cbind) > 0 {
		sb.WriteString("\tcbind=")
		sb.WriteString(base64.StdEncoding.EncodeToString(cbind))
	}
	line, err := c.exchange(id, sb.String())
	if err != nil {
		return nil, nil, err
	}
	x := &SASLExchange{c: c, id: id}
	step, err := parseSASLStep(line)
	return x, step, err
}

// Next carries one client response into the exchange.
func (x *SASLExchange) Next(response []byte) (*SASLStep, error) {
	line, err := x.c.exchange(x.id, fmt.Sprintf("CONT\t%s\t%s", x.id, base64.StdEncoding.EncodeToString(response)))
	if err != nil {
		return nil, err
	}
	return parseSASLStep(line)
}

// Cancel abandons an exchange the mail client walked away from, so the service
// does not hold state for a conversation nobody will finish.
func (x *SASLExchange) Cancel() {
	_, _ = x.c.exchange(x.id, fmt.Sprintf("CANCEL\t%s", x.id))
}

func parseSASLStep(line string) (*SASLStep, error) {
	fields := strings.Split(line, "\t")
	if len(fields) == 0 {
		return nil, fmt.Errorf("auth/client: empty sasl reply")
	}
	if fields[0] == "CONT" {
		if len(fields) < 3 {
			return nil, fmt.Errorf("auth/client: malformed continuation %q", line)
		}
		challenge, err := base64.StdEncoding.DecodeString(fields[2])
		if err != nil {
			return nil, fmt.Errorf("auth/client: continuation is not base64: %w", err)
		}
		return &SASLStep{Challenge: challenge}, nil
	}
	res, err := parseAuthResponse(line)
	if err != nil {
		return nil, err
	}
	step := &SASLStep{Done: true, Result: res}
	for _, f := range fields[2:] {
		if strings.HasPrefix(f, "resp=") {
			final, derr := base64.StdEncoding.DecodeString(strings.TrimPrefix(f, "resp="))
			if derr != nil {
				return nil, fmt.Errorf("auth/client: server-final is not base64: %w", derr)
			}
			step.Final = final
		}
	}
	return step, nil
}
