package protocol

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-sasl"
)

// SASL mechanisms this service runs itself. A session relays the bytes; it
// does not read a verifier, which is what keeps the passdb out of it (#1733).
const (
	MechPlain           = "PLAIN"
	MechLogin           = "LOGIN"
	MechScramSha1       = "SCRAM-SHA-1"
	MechScramSha1Plus   = "SCRAM-SHA-1-PLUS"
	MechScramSha256     = "SCRAM-SHA-256"
	MechScramSha256Plus = "SCRAM-SHA-256-PLUS"
)

// saslExchange is one multi-step SASL conversation, held per connection and
// keyed by the request id the session chose.
type saslExchange struct {
	mech string
	// deadline is when this conversation is dropped unfinished.
	deadline time.Time
	// server runs the mechanism; username is filled by the lookup closure, the
	// only place the name appears before the exchange finishes.
	server   sasl.Server
	username string
	service  string
	rip      string
	session  string
}

// SASLExchangeTTL bounds how long an unfinished conversation is kept. A client
// that walks away must not leave the service holding its half: the relay's
// connection is one per mail process, so state kept on it is kept for the
// life of that process (#1733).
const SASLExchangeTTL = 30 * time.Second

// exchanges holds the in-flight conversations of one connection. A conversation
// spans several commands, so it cannot live in a handler's stack.
type exchanges struct {
	mu  sync.Mutex
	m   map[string]*saslExchange
	ttl time.Duration
	now func() time.Time
}

func newExchanges() *exchanges {
	return &exchanges{m: map[string]*saslExchange{}, ttl: SASLExchangeTTL, now: time.Now}
}

func (e *exchanges) put(id string, x *saslExchange) {
	e.mu.Lock()
	defer e.mu.Unlock()
	x.deadline = e.now().Add(e.ttl)
	e.expireLocked()
	e.m[id] = x
}

// expireLocked drops conversations past their deadline. Done on every touch
// rather than by a sweeper: the map is small and the cost is bounded by it.
func (e *exchanges) expireLocked() {
	now := e.now()
	for id, x := range e.m {
		if now.After(x.deadline) {
			delete(e.m, id)
		}
	}
}

// take returns a live conversation and extends it: a client still stepping
// through one is not the client this deadline is aimed at.
func (e *exchanges) take(id string) (*saslExchange, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expireLocked()
	x, ok := e.m[id]
	if ok {
		x.deadline = e.now().Add(e.ttl)
	}
	return x, ok
}

func (e *exchanges) drop(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.m, id)
}

// scramMechanisms names the SCRAM mechanisms the chain's drivers can serve at
// all. Whether a given user has a verifier is a per-user fact, read during the
// exchange -- a handshake does not know who is calling yet.
func (s *Server) scramMechanisms() []string {
	var out []string
	if s.chainHasSCRAM(false) {
		out = append(out, MechScramSha1, MechScramSha1Plus)
	}
	if s.chainHasSCRAM(true) {
		out = append(out, MechScramSha256, MechScramSha256Plus)
	}
	return out
}

func (s *Server) chainHasSCRAM(sha256 bool) bool {
	for _, db := range s.passdbs {
		if sha256 {
			if _, ok := db.(SCRAMSha256Lookup); ok {
				return true
			}
			continue
		}
		if _, ok := db.(SCRAMSha1Lookup); ok {
			return true
		}
	}
	return false
}

// startSCRAM begins a SCRAM conversation. cbData is the channel binding the
// session holds: it terminates TLS, so the bytes can only come from there.
func (s *Server) startSCRAM(mech string, cbData []byte) (*saslExchange, error) {
	chain := &chainAuthenticator{chain: s.passdbs}
	x := &saslExchange{mech: mech}
	sha256Lookup := func(user string) (*sasl.ScramCredentials, error) {
		x.username = user
		return chain.LookupSCRAMSha256(user)
	}
	sha1Lookup := func(user string) (*sasl.ScramCredentials, error) {
		x.username = user
		return chain.LookupSCRAMSha1(user)
	}
	switch mech {
	case MechScramSha256:
		x.server = sasl.NewScramSha256Server(sha256Lookup)
	case MechScramSha256Plus:
		if len(cbData) == 0 {
			return nil, fmt.Errorf("%s needs channel binding data", mech)
		}
		x.server = sasl.NewScramSha256PlusServer(sha256Lookup, sasl.ScramServerOptions{ChannelBindingData: cbData})
	case MechScramSha1:
		x.server = sasl.NewScramSha1Server(sha1Lookup)
	case MechScramSha1Plus:
		if len(cbData) == 0 {
			return nil, fmt.Errorf("%s needs channel binding data", mech)
		}
		x.server = sasl.NewScramSha1PlusServer(sha1Lookup, sasl.ScramServerOptions{ChannelBindingData: cbData})
	default:
		return nil, fmt.Errorf("mechanism %q is not a SCRAM one", mech)
	}
	return x, nil
}

// step runs one round of the conversation and says what the wire should carry.
func (x *saslExchange) step(response []byte) (challenge []byte, done bool, err error) {
	return x.server.Next(response)
}

// user is the name the exchange authenticated, known only once the client's
// first message has been read.
func (x *saslExchange) user() string { return x.username }

func decodeResp(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	return base64.StdEncoding.DecodeString(s)
}

func encodeChallenge(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

// writeContinue sends the server's challenge for a conversation still running.
func writeContinue(conn net.Conn, id string, challenge []byte) {
	fmt.Fprintf(conn, "CONT\t%s\t%s\n", id, encodeChallenge(challenge))
}

func isSCRAM(mech string) bool { return strings.HasPrefix(mech, "SCRAM-") }

// beginSCRAM starts a conversation the session relays. The reply is a CONT
// challenge, or the same OK/FAIL a PLAIN login ends with.
func (s *Server) beginSCRAM(conn net.Conn, live *exchanges, id, mech, service, resp, cbind, rip, sessionID string) string {
	cbData, err := decodeResp(cbind)
	if err != nil {
		fmt.Fprintf(conn, "FAIL\t%s\treason=bad-credentials\n", id)
		return "bad_request"
	}
	x, err := s.startSCRAM(mech, cbData)
	if err != nil {
		fmt.Fprintf(conn, "FAIL\t%s\treason=%s\n", id, err)
		return "bad_request"
	}
	x.service, x.rip, x.session = service, rip, sessionID
	first, err := decodeResp(resp)
	if err != nil {
		fmt.Fprintf(conn, "FAIL\t%s\treason=bad-credentials\n", id)
		return "bad_request"
	}
	live.put(id, x)
	return s.advanceSCRAM(conn, live, id, x, first)
}

// handleContinue carries one client response into a conversation already open.
func (s *Server) handleContinue(conn net.Conn, live *exchanges, fields []string) string {
	if len(fields) < 2 {
		return "bad_request"
	}
	id := fields[1]
	x, ok := live.take(id)
	if !ok {
		fmt.Fprintf(conn, "FAIL\t%s\treason=no-such-exchange\n", id)
		return "bad_request"
	}
	var raw string
	if len(fields) > 2 {
		raw = fields[2]
	}
	response, err := decodeResp(raw)
	if err != nil {
		live.drop(id)
		fmt.Fprintf(conn, "FAIL\t%s\treason=bad-credentials\n", id)
		return "bad_request"
	}
	return s.advanceSCRAM(conn, live, id, x, response)
}

// advanceSCRAM runs one round and answers the wire. A finished exchange leaves
// nothing behind: the id is free again, and a late CONT finds no conversation.
func (s *Server) advanceSCRAM(conn net.Conn, live *exchanges, id string, x *saslExchange, response []byte) string {
	challenge, done, err := x.step(response)
	if err != nil {
		live.drop(id)
		if s.failureDelay > 0 {
			time.Sleep(s.failureDelay)
		}
		slog.Info("auth: sasl exchange failed",
			"sid", x.session, "proto", x.service, "mech", x.mech, "user", x.user(), "result", "fail")
		fmt.Fprintf(conn, "FAIL\t%s\n", id)
		return "fail"
	}
	if !done {
		writeContinue(conn, id, challenge)
		return "cont"
	}
	live.drop(id)
	user := x.user()
	if len(challenge) > 0 {
		// The server-final message travels with the verdict: a client that
		// verifies the server signature needs both, and a second round trip
		// after success is a round trip for nothing.
		fmt.Fprintf(conn, "OK\t%s\tuser=%s\tsession=%s\tservice=%s\tresp=%s\n",
			id, user, x.session, x.service, encodeChallenge(challenge))
	} else {
		fmt.Fprintf(conn, "OK\t%s\tuser=%s\tsession=%s\tservice=%s\n", id, user, x.session, x.service)
	}
	slog.Info("auth: sasl exchange succeeded",
		"sid", x.session, "proto", x.service, "mech", x.mech, "user", user, "result", "ok")
	return "ok"
}
