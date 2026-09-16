package client

import "fmt"

// RelayServer is a sasl.Server that carries bytes to the auth service and back.
// A session holds no verifier and runs no mechanism: that is the point (#1733).
type RelayServer struct {
	c        *Client
	mech     string
	service  string
	remoteIP string
	session  string
	cbind    []byte

	x    *SASLExchange
	done bool
	// Result is the service's verdict once the exchange finishes.
	Result *AuthResult
	// OnSuccess runs after a clean finish; a non-nil return fails the login.
	OnSuccess func(res *AuthResult) error
}

// NewRelayServer builds the relay for one mechanism. cbind is the channel
// binding the session holds, which is why -PLUS can only be run from here.
func NewRelayServer(c *Client, mech, service, remoteIP, sessionID string, cbind []byte) *RelayServer {
	return &RelayServer{c: c, mech: mech, service: service, remoteIP: remoteIP, session: sessionID, cbind: cbind}
}

// Next implements sasl.Server: one round of the relayed conversation.
func (r *RelayServer) Next(response []byte) (challenge []byte, done bool, err error) {
	var step *SASLStep
	if r.x == nil {
		r.x, step, err = r.c.BeginSASL(r.mech, r.service, r.remoteIP, r.session, r.cbind, response)
	} else {
		step, err = r.x.Next(response)
	}
	if err != nil {
		r.done = true
		return nil, true, err
	}
	if !step.Done {
		return step.Challenge, false, nil
	}
	r.done = true
	r.Result = step.Result
	if step.Result == nil {
		return nil, true, fmt.Errorf("auth/relay: the service returned no verdict")
	}
	if r.OnSuccess != nil {
		if herr := r.OnSuccess(step.Result); herr != nil {
			return nil, true, herr
		}
	}
	// The server-final message travels with the verdict, so a client that
	// verifies the server signature still gets it.
	return step.Final, true, nil
}

// Cancel frees a conversation the mail client abandoned, so the service does
// not hold its half until the deadline. Safe to call on a finished exchange.
func (r *RelayServer) Cancel() {
	if r.x == nil || r.done {
		return
	}
	r.done = true
	r.x.Cancel()
}
