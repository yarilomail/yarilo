// Package login is the mail-protocol login proxy: authenticate, resolve the
// backend, proxy the session. TLS terminates here, never at the backend.
package login

import (
	"bufio"
	"context"
	crand "crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	proxyproto "github.com/pires/go-proxyproto"

	authclient "github.com/yarilomail/yarilo/internal/auth/client"
	"github.com/yarilomail/yarilo/internal/cluster/proto"
	"github.com/yarilomail/yarilo/internal/loginproto"
	"github.com/yarilomail/yarilo/internal/warden"
)

// Protocol identifies the mail protocol handled by the login pod.
type Protocol string

const (
	ProtocolIMAP        Protocol = "imap"
	ProtocolIMAPS       Protocol = "imaps"
	ProtocolPOP3        Protocol = "pop3"
	ProtocolPOP3S       Protocol = "pop3s"
	ProtocolSubmission  Protocol = "submission"
	ProtocolSubmissions Protocol = "submissions"
	ProtocolManageSieve Protocol = "managesieve"
)

// Base collapses a listener protocol to its backend name (imaps→imap): the
// granularity the director counts sessions at, and sends on LOOKUP.
func (p Protocol) Base() string {
	switch p {
	case ProtocolIMAPS:
		return "imap"
	case ProtocolPOP3S:
		return "pop3"
	case ProtocolSubmissions:
		return "submission"
	default:
		return string(p)
	}
}

// Options configures the login proxy Server.
type Options struct {
	// Protocol is one of the Protocol constants above.
	Protocol Protocol
	// Tag restricts director LOOKUP to backends with this tag.
	// "" means the untagged pool, not "any tag".
	Tag string
	// DirectorAddr is the host:port of yarilo-director (e.g. "yarilo-director:9102").
	// Ignored when BackendAddr is set.
	DirectorAddr string
	// DirectorTLS is the mTLS config for connecting to yarilo-director.
	// Nil means plain TCP.
	DirectorTLS *tls.Config
	// BackendAddr routes every session to one fixed address, bypassing the
	// director; DirectorAddr and Tag are then unused.
	BackendAddr string
	// LocalIP is the pod IP used in the ME handshake with the director.
	LocalIP string
	// BackendPort is the containerPort on backend pods.
	BackendPort int
	// BackendTLS is the mTLS config for connecting to backend pods.
	// Nil means plain TCP.
	BackendTLS *tls.Config
	// ExtTLS is the client-facing config for implicit-TLS listeners (993, 995,
	// 465). Nil leaves the listener plain on accept.
	ExtTLS *tls.Config
	// StarttlsTLS is what STARTTLS / STLS upgrades to during the preamble.
	// Nil means the listener does not advertise it.
	StarttlsTLS *tls.Config
	// WardenAddr is the host:port of yarilo-warden for per-user@IP connection
	// limiting (mail_max_userip_connections). Empty = no limit enforcement.
	WardenAddr string
	// WardenTLS is the mTLS config for connecting to yarilo-warden.
	// Nil means plain TCP.
	WardenTLS *tls.Config
	// WardenFailOpen controls what happens when yarilo-warden is unreachable.
	// true = allow the session (fail open); false = reject the session (fail closed).
	WardenFailOpen bool
	// TransientRetries is the extra-attempt budget for transient failures
	// (auth temp-fail, auth dial, backend bring-up). 0 selects the default (3).
	TransientRetries int
	// TransientReloginCap is how many transient failures one connection may
	// answer with a tagged NO. Independent of AuthMaxAttempts; 0 = 3.
	TransientReloginCap int
	// WardenConns sizes the shared warden pool; 0 takes the default. The
	// protocol has no request id, so one connection serves one command.
	WardenConns int
	// DialRetries is the number of attempts (with exponential backoff) when
	// dialling external dependencies at startup. 0 or 1 means a single attempt.
	DialRetries int

	// LookupHoldMax / LookupHoldBackoff bound the confirmed-kick retry: their
	// product must exceed the director's worst-case confirm time. 0 = 20/150ms.

	// SessionSyncInterval paces the session list sent to the director. Zero
	// takes the default; negative sends it only on a (re)connect (#1393).
	SessionSyncInterval time.Duration
	LookupHoldMax       int
	LookupHoldBackoff   time.Duration

	// AuthAddr is the host:port of yarilo-auth (e.g. "yarilo-auth:9100").
	// Required: if empty every login attempt is rejected with a temporary error.
	AuthAddr string
	// AuthTLS is the mTLS config for connecting to yarilo-auth.
	// Nil means plain TCP.
	AuthTLS *tls.Config

	// AuthMaxAttempts is the failed authentications one connection may make
	// before it is closed; 0 = 3. Mirrors auth_max_attempts.
	AuthMaxAttempts int

	// OAuth2Enabled advertises and accepts OAUTHBEARER and XOAUTH2 mechanisms.
	// Mirrors cfg.Auth.OAuth2 being non-empty.
	OAuth2Enabled bool
	// DisablePlainAuth suppresses PLAIN and LOGIN from pre-TLS capability
	// advertisements. After STARTTLS/implicit-TLS they are always offered.
	DisablePlainAuth bool
	// SieveExtensions is the space-joined list of supported Sieve extensions
	// advertised in the ManageSieve SIEVE capability line.
	SieveExtensions string
	// SieveMaxInvalidCmds is the number of unrecognised pre-auth commands
	// after which the server disconnects with BYE.
	SieveMaxInvalidCmds int

	// HAProxy enables PROXY protocol v1/v2 header reading from trusted upstreams.
	HAProxy        bool
	HAProxyTimeout time.Duration
	HAProxyNets    []*net.IPNet

	// XClient enables inbound client-IP forwarding (IMAP ID, XCLIENT). Off
	// means ID replies NIL and XCLIENT is an unknown command.
	XClient bool
	// XClientNets are the CIDRs whose forwarded client IP is trusted: the
	// socket peer must sit inside one. Empty trusts nobody.
	XClientNets []*net.IPNet
}

// liveSession tracks one active proxied session for kick support.
type liveSession struct {
	id          string
	user        string
	backendConn net.Conn
	// clientConn is the other leg, and a kick must close both: a client that
	// says nothing for minutes would otherwise keep the proxy alive (#1366).
	clientConn net.Conn
	// backendIP and proto are what the director was told, kept so the session
	// can be announced again when the director that heard it dies (#1393).
	backendIP string
	proto     string
}

// watchConn is the persistent director connection. LOOKUP rides it too, the
// reply routed by request id, which spares every login a dial and handshake.
type watchConn struct {
	mu sync.Mutex
	c  *proto.Conn

	pendMu  sync.Mutex
	pending map[string]chan string
}

// awaitReply registers id and returns the channel its reply will arrive on.
func (w *watchConn) awaitReply(id string) chan string {
	ch := make(chan string, 1)
	w.pendMu.Lock()
	if w.pending == nil {
		w.pending = make(map[string]chan string)
	}
	w.pending[id] = ch
	w.pendMu.Unlock()
	return ch
}

func (w *watchConn) forgetReply(id string) {
	w.pendMu.Lock()
	delete(w.pending, id)
	w.pendMu.Unlock()
}

// deliver routes a reply to its waiter. Reports false when nobody is waiting;
// ordinary push lines fall through to the watch handling.
func (w *watchConn) deliver(id, line string) bool {
	w.pendMu.Lock()
	ch, ok := w.pending[id]
	if ok {
		delete(w.pending, id)
	}
	w.pendMu.Unlock()
	if !ok {
		return false
	}
	ch <- line
	return true
}

// failPending wakes every waiter when the connection dies; an in-flight login
// falls back to a fresh dial instead of waiting out its timeout.
func (w *watchConn) failPending() {
	w.pendMu.Lock()
	pending := w.pending
	w.pending = nil
	w.pendMu.Unlock()
	for _, ch := range pending {
		close(ch)
	}
}

// lookup performs a LOOKUP over the persistent connection.
func (w *watchConn) lookup(id, username, tag, protoName string, timeout time.Duration) (proto.LookupResult, error) {
	ch := w.awaitReply(id)

	w.mu.Lock()
	err := w.c.WriteLine(proto.LookupRequestLine(id, username, tag, protoName))
	w.mu.Unlock()
	if err != nil {
		w.forgetReply(id)
		return proto.LookupResult{}, fmt.Errorf("director lookup: write: %w", err)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case line, ok := <-ch:
		if !ok {
			return proto.LookupResult{}, errWatchClosed
		}
		return proto.ParseLookupReply(line)
	case <-timer.C:
		w.forgetReply(id)
		return proto.LookupResult{}, fmt.Errorf("director lookup: timed out after %s", timeout)
	}
}

// errWatchClosed means the watch connection dropped while a lookup was in
// flight. Safe to retry on a fresh connection: LOOKUP is a read.
var errWatchClosed = errors.New("director watch connection closed")

// directorLookupTimeout bounds one LOOKUP over the persistent connection;
// a wedged connection falls back to a dial rather than holding the login.
const directorLookupTimeout = 10 * time.Second

func (w *watchConn) sessionOpen(sessID, username, backendIP, protoName string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.WriteLine(fmt.Sprintf("SESSION-OPEN\t%s\t%s\t%s\t%s", sessID, proto.TabEscape(username), backendIP, protoName))
}

// Framed START ... END so a truncated list is not mistaken for a complete
// one: applying half of it would erase live sessions.
func (w *watchConn) sessionSyncStart() {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.WriteLine("SESSION-SYNC-START")
}

func (w *watchConn) sessionSyncChunk(ids []string) {
	if len(ids) == 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.WriteLine("SESSION-SYNC\t" + strings.Join(ids, "\t"))
}

func (w *watchConn) sessionSyncEnd() {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.WriteLine("SESSION-SYNC-END")
}

func (w *watchConn) sessionClose(sessID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.WriteLine(fmt.Sprintf("SESSION-CLOSE\t%s", sessID))
}

func (w *watchConn) pong() {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.WriteLine("PONG")
}

// sessionIDAlphabet is the 52-character Postfix set. 'z' separates the time
// and sequence parts, so the sequence uses only the first 51.
const sessionIDAlphabet = "0123456789BCDFGHJKLMNPQRSTVWXYZbcdfghjklmnpqrstvwxyz"

// encodeSessionPart encodes n in the given alphabet, left-padding with
// alphabet[0] to at least minLen characters.
func encodeSessionPart(n uint64, alphabet string, minLen int) string {
	base := uint64(len(alphabet))
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = alphabet[n%base]
		n /= base
	}
	for len(buf)-pos < minLen {
		pos--
		buf[pos] = alphabet[0]
	}
	return string(buf[pos:])
}

// Server is the login proxy server.
type Server struct {
	opts    Options
	reqID   atomic.Uint64
	sessSeq atomic.Uint64
	seed    string // 4 base51 chars, random per Server instance

	sessMu sync.RWMutex
	// announceMu orders what the director hears: a list can never be built
	// before a session exists and sent after its SESSION-OPEN (#1393).
	announceMu sync.Mutex
	sessions   map[string][]*liveSession // username → active sessions

	watchMu sync.RWMutex
	watch   *watchConn // persistent director connection for push notifications

	// Shared auth client: the wire carries a request id, so logins multiplex
	// over one connection. Lazy, since the pod may start before the service.
	authMu sync.Mutex
	authCl *authclient.Client

	// Shared warden pool: every command carries the session id, so a small
	// fixed pool serves them all. Lazy, like the auth client.
	wardenMu   sync.Mutex
	wardenPool *warden.Pool

	// Graceful-drain state: Shutdown closes the listeners and waits; draining
	// turns the resulting Accept error into a clean return.
	drainMu   sync.Mutex
	listeners []net.Listener
	draining  bool
	inflight  sync.WaitGroup
}

// authClient returns the shared yarilo-auth client, dialling it on first use.
// A dial failure leaves the field nil so the next login retries.
func (s *Server) authClient() (*authclient.Client, error) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.authCl != nil {
		return s.authCl, nil
	}
	cl, err := authclient.Dial(s.opts.AuthAddr, s.opts.AuthTLS)
	if err != nil {
		return nil, err
	}
	s.authCl = cl
	return cl, nil
}

// wardenClient returns the shared warden pool, creating it on first use.
// The pool dials lazily; a dial error surfaces on the first command.
func (s *Server) wardenClient() *warden.Pool {
	s.wardenMu.Lock()
	defer s.wardenMu.Unlock()
	if s.wardenPool == nil {
		s.wardenPool = warden.NewPool(s.opts.WardenAddr, s.opts.WardenTLS, s.opts.WardenConns, 0)
	}
	return s.wardenPool
}

func (s *Server) closeWardenPool() {
	s.wardenMu.Lock()
	pool := s.wardenPool
	s.wardenPool = nil
	s.wardenMu.Unlock()
	if pool != nil {
		pool.Close()
	}
}

func (s *Server) closeAuthClient() {
	s.authMu.Lock()
	cl := s.authCl
	s.authCl = nil
	s.authMu.Unlock()
	if cl != nil {
		_ = cl.Close()
	}
}

// defaultTransientRetries is the extra-attempt budget for a transient failure.
const defaultTransientRetries = 3

// transientRetryBackoff is the pause between attempts. Short: the failures it
// covers (pod rolling, connection re-established) resolve in under a second.
const transientRetryBackoff = 150 * time.Millisecond

func (s *Server) transientRetries() int {
	if s.opts.TransientRetries > 0 {
		return s.opts.TransientRetries
	}
	if s.opts.TransientRetries < 0 {
		return 0 // explicit opt-out
	}
	return defaultTransientRetries
}

// defaultTransientReloginCap is the client-side re-LOGIN budget per connection.
// Distinct from transientRetries, the per-hop internal budget.
const defaultTransientReloginCap = 3

func (s *Server) transientReloginCap() int {
	if s.opts.TransientReloginCap > 0 {
		return s.opts.TransientReloginCap
	}
	if s.opts.TransientReloginCap < 0 {
		return 0 // explicit opt-out: close on the first transient
	}
	return defaultTransientReloginCap
}

// newSessionID returns a Postfix-style long queue ID. The per-Server seed
// makes it unique across pods; 'z' stays the unambiguous separator.
func (s *Server) newSessionID() string {
	now := time.Now()
	secs := uint64(now.Unix())
	usec := uint64(now.Nanosecond() / 1000)
	seq := s.sessSeq.Add(1)
	return encodeSessionPart(secs, sessionIDAlphabet, 6) +
		encodeSessionPart(usec, sessionIDAlphabet, 4) +
		"z" +
		s.seed +
		encodeSessionPart(seq, sessionIDAlphabet[:51], 1)
}

// New creates a Server.
func New(opts Options) *Server {
	var b [3]byte
	_, _ = crand.Read(b[:])
	seed := uint64(b[0])<<16 | uint64(b[1])<<8 | uint64(b[2])
	return &Server{
		opts:     opts,
		seed:     encodeSessionPart(seed, sessionIDAlphabet[:51], 4),
		sessions: make(map[string][]*liveSession),
	}
}

// Serve accepts connections on ln until the listener is closed.
func (s *Server) Serve(ln net.Listener) error {
	if s.opts.HAProxy {
		timeout := s.opts.HAProxyTimeout
		if timeout == 0 {
			timeout = 3 * time.Second
		}
		ln = &proxyproto.Listener{
			Listener:          ln,
			Policy:            haProxyPolicy(s.opts.HAProxyNets),
			ReadHeaderTimeout: timeout,
		}
	}
	s.drainMu.Lock()
	if s.draining {
		s.drainMu.Unlock()
		_ = ln.Close()
		return nil
	}
	s.listeners = append(s.listeners, ln)
	s.drainMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.startKickSubscriber(ctx)
	for {
		conn, err := ln.Accept()
		if err != nil {
			s.drainMu.Lock()
			draining := s.draining
			s.drainMu.Unlock()
			if draining {
				return nil // listener closed by Shutdown — clean stop
			}
			return fmt.Errorf("login: accept: %w", err)
		}
		s.inflight.Add(1)
		go func() {
			defer s.inflight.Done()
			s.handleConn(conn)
		}()
	}
}

// Shutdown stops accepting and waits for in-flight sessions until ctx
// expires; whatever is left goes with the process. Idempotent.
func (s *Server) Shutdown(ctx context.Context) error {
	s.drainMu.Lock()
	if s.draining {
		s.drainMu.Unlock()
		return nil
	}
	s.draining = true
	lns := s.listeners
	s.listeners = nil
	s.drainMu.Unlock()
	for _, ln := range lns {
		_ = ln.Close()
	}
	done := make(chan struct{})
	go func() { s.inflight.Wait(); close(done) }()
	select {
	case <-done:
		s.closeAuthClient()
		s.closeWardenPool()
		return nil
	case <-ctx.Done():
		s.closeAuthClient()
		s.closeWardenPool()
		return ctx.Err() // grace expired with sessions still live
	}
}

// loginOutcome is the result of one login pass: proxy, close, or keep the
// connection open for the client to retry.
type loginOutcome int

const (
	outcomeDone  loginOutcome = iota // authenticated and brought up; proxy it
	outcomeClose                     // fatal (bad-password exhausted, over limit); close
	outcomeRetry                     // transient; a tagged NO is sent, keep the connection for a re-LOGIN
)

// established is what a successful login pass hands back to handleConn to
// proxy and own for the session's lifetime.
type established struct {
	bs *backendSession
	// user is the identity the auth service resolved, not the string typed:
	// everything keyed by identity reads it here, once (#1306).
	user          string
	releaseWarden func() // warden heartbeat-cancel + Disconnect; nil when warden is disabled
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(60 * time.Second)) //nolint:errcheck

	remoteAddr := conn.RemoteAddr().String()
	clientIP, remotePort, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		clientIP = remoteAddr
		remotePort = ""
	}

	sessID := s.newSessionID()
	log := slog.With("sid", sessID, "proto", string(s.opts.Protocol), "remote_ip", clientIP, "remote_port", remotePort)
	log.Info("login: connect")

	// Implicit-TLS upgrade (IMAPS / POP3S / Submissions).
	if s.opts.ExtTLS != nil {
		tlsStart := time.Now()
		tlsConn := tls.Server(conn, s.opts.ExtTLS)
		if err := tlsConn.Handshake(); err != nil {
			log.Debug("login: tls handshake", "err", err)
			s.incResult("tls_error")
			return
		}
		s.observePhase(phaseTLSHandshake, tlsStart)
		conn = tlsConn
	}

	rd := bufio.NewReaderSize(conn, 4096)

	// Extract preamble: speak the protocol pre-auth exchange to collect credentials.
	// authConn/authRd may be TLS-upgraded from the original conn/rd if STARTTLS happened.
	preambleStart := time.Now()
	pre, authConn, authRd, err := extractPreamble(conn, rd, s.opts.Protocol, s.opts.StarttlsTLS, s.opts, relayContext{dial: s.authClient, sessionID: sessID})
	if err != nil {
		log.Debug("login: preamble", "err", err)
		s.incResult("preamble_error")
		return
	}
	s.observePhase(phasePreamble, preambleStart)

	// attempt runs one authenticate→route→bring-up pass, releasing whatever it
	// acquired on a transient failure so the client may LOGIN again.
	var est *established
	attempt := func() (loginOutcome, *established) {
		// committed flips only on a successful bring-up; until then the unwind
		// releases the warden slot, afterwards handleConn owns it.
		committed := false
		var releaseWarden func()
		defer func() {
			if !committed && releaseWarden != nil {
				releaseWarden()
			}
		}()

		// Authenticate via yarilo-auth: passdb chain, brute-force penalty, token issuance.
		if s.opts.AuthAddr == "" {
			// Permanent misconfiguration, not a transient blip: close rather than
			// hold the socket open for the re-login budget.
			log.Error("login: auth_addr not configured")
			// Closing path: announce the close, not a keep-open transient code.
			writeProtoClose(authConn, s.opts.Protocol, "service temporarily unavailable")
			s.incResult("unavailable")
			return outcomeClose, nil
		}
		// Shared multiplexed client — no per-login handshake. The phase metric
		// reads ~0 except on the first login after a pod start or auth reconnect.
		authDialStart := time.Now()
		var authCl *authclient.Client
		for attempt := 0; ; attempt++ {
			var derr error
			authCl, derr = s.authClient()
			if derr == nil {
				break
			}
			if attempt >= s.transientRetries() {
				log.Error("login: yarilo-auth dial", "addr", s.opts.AuthAddr, "attempts", attempt+1, "err", derr)
				writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "service temporarily unavailable")
				// Observed on failure too: a timed-out dial must show in the
				// histogram.
				s.observePhase(phaseAuthDial, authDialStart)
				s.incTransientExhausted(stageAuthDial)
				s.incResult("unavailable")
				return outcomeRetry, nil
			}
			log.Warn("login: yarilo-auth dial failed, retrying", "addr", s.opts.AuthAddr, "attempt", attempt+1, "err", derr)
			s.incTransientRetry(stageAuthDial)
			time.Sleep(transientRetryBackoff)
		}
		s.observePhase(phaseAuthDial, authDialStart)

		// The connection stays open after a bad password, up to maxAuthAttempts;
		// the last one is answered and then announced closed.
		maxAuthAttempts := authAttemptLimit(s.opts)
		var authResult *authclient.AuthResult
		for attempt := 1; ; attempt++ {
			// The one place a forwarded address replaces the socket IP, and only
			// from a trusted peer; at the loop top so a later forward counts.
			if pre.forwardIP != "" && clientIP != pre.forwardIP {
				if ipInNets(clientIP, s.opts.XClientNets) {
					log = log.With("orig_ip", clientIP, "fwd_ip", pre.forwardIP, "fwd_port", pre.forwardPort, "fwd_via", pre.forwardSource)
					log.Info("login: client ip forwarded")
					clientIP = pre.forwardIP
				} else if pre.forwardSource == "xclient" {
					// An untrusted peer claiming to be a proxy is an anomaly.
					log.Warn("login: ignoring XCLIENT from untrusted peer", "peer_ip", clientIP, "claimed_ip", pre.forwardIP)
					pre.forwardIP = ""
				} else {
					// Bare IMAP ID with x-originating-ip is routine MUA chatter;
					// Debug to avoid log spam.
					log.Debug("login: ignoring forwarded ID from untrusted peer", "peer_ip", clientIP, "claimed_ip", pre.forwardIP)
					pre.forwardIP = ""
				}
			}

			var aerr error
			// A relayed exchange already proved the identity and has no password
			// to re-send: a second call would be a second verdict (#1733).
			if pre.authResult != nil {
				authResult = pre.authResult
			}
			// Retry temp-fails here rather than surfacing a passdb blip. Safe to
			// repeat: internal failures do not touch the auth-penalty counter.
			for tfAttempt := 0; pre.authResult == nil; tfAttempt++ {
				authStart := time.Now()
				authResult, aerr = authCl.AuthenticateAs(pre.authzid, pre.username, pre.password, wardenService(s.opts.Protocol), clientIP, sessID)
				// One observation per attempt: each is its own round-trip.
				s.observePhase(phaseAuth, authStart)
				if !errors.Is(aerr, authclient.ErrTempFail) {
					break
				}
				if tfAttempt >= s.transientRetries() {
					log.Warn("login: auth temp fail", "user", pre.username, "attempts", tfAttempt+1, "result", "fail")
					writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "service temporarily unavailable")
					s.incTransientExhausted(stageAuth)
					s.incResult("unavailable")
					return outcomeRetry, nil
				}
				log.Warn("login: auth temp fail, retrying", "user", pre.username, "attempt", tfAttempt+1)
				s.incTransientRetry(stageAuth)
				time.Sleep(transientRetryBackoff)
			}

			authFailed := aerr != nil
			if aerr == nil {
				if authResult.Nologin {
					log.Info("login: auth", "user", pre.username, "result", "fail", "reason", "nologin", "attempt", attempt)
					authFailed = true
				} else if authResult.AllowNets != "" && !checkAllowNets(clientIP, authResult.AllowNets) {
					log.Info("login: auth", "user", pre.username, "result", "fail", "reason", "ip_not_in_allow_nets", "attempt", attempt)
					authFailed = true
				}
			} else {
				log.Info("login: auth", "user", pre.username, "result", "fail", "attempt", attempt)
			}

			if !authFailed {
				log.Info("login: auth", "user", pre.username, "result", "ok", "attempt", attempt)
				// The backend's preamble verifies this token: dialling without
				// one blames the backend for the auth service's answer (#1733).
				if authResult != nil && authResult.Token == "" {
					log.Error("login: auth returned no session token; refusing before the backend",
						"user", pre.username, "result", "fail")
					writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "service temporarily unavailable")
					s.incResult("unavailable")
					return outcomeClose, nil
				}
				break
			}

			if attempt >= maxAuthAttempts || !isRetriableProtocol(s.opts.Protocol) {
				// Tagged NO [AUTHENTICATIONFAILED] then * BYE: the last LOGIN must
				// get a tagged reply before the close announcement.
				writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeAuthenticationFail, "Too many failed authentications")
				writeProtoClose(authConn, s.opts.Protocol, "closing")
				return outcomeClose, nil
			}
			writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeAuthenticationFail, "Authentication failed.")

			var retryExtTLS *tls.Config
			if _, ok := authConn.(*tls.Conn); !ok {
				retryExtTLS = s.opts.StarttlsTLS
			}
			pre, authConn, authRd, err = continueAuth(authConn, authRd, retryExtTLS, s.opts.Protocol, s.opts, relayContext{dial: s.authClient, sessionID: sessID})
			if err != nil {
				log.Debug("login: preamble retry", "err", err)
				return outcomeClose, nil
			}
			log.Info("login: auth retry", "user", pre.username, "attempt", attempt+1)
		}

		// The session belongs to the resolved identity, not the typed string:
		// they differ for a master login, and the backend's VERIFY knows it.
		authUser := resolvedIdentity(authResult, pre.username)
		if authUser != pre.username {
			log.Info("login: acting as the resolved identity",
				"claimed", pre.username, "user", authUser)
		}

		// Find backend address: fixed addr (standalone) or director LOOKUP.
		// tag is hoisted so the fast-fail re-route below can re-LOOKUP with it.
		var backendAddr, tag string
		if s.opts.BackendAddr != "" {
			backendAddr = s.opts.BackendAddr
		} else {
			// Per-user director_tag from passdb/userdb wins over the static Tag
			// config, so a shared login fleet can route users to different pools.
			tag = s.opts.Tag
			if authResult.DirectorTag != "" {
				tag = authResult.DirectorTag
			}
			var err error
			lookupStart := time.Now()
			backendAddr, err = s.directorLookupWithHold(authUser, tag, log)
			s.observePhase(phaseDirectorLookup, lookupStart)
			if err != nil {
				log.Warn("login: director lookup failed", "user", pre.username, "err", err)
				writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "backend unavailable")
				s.incResult("unavailable")
				return outcomeRetry, nil
			}
		}

		// Warden connection limit check over the shared pool (no per-session
		// dial); the phase metric measures one round trip.
		if s.opts.WardenAddr != "" {
			wardenStart := time.Now()
			ap := s.wardenClient()
			svc := wardenService(s.opts.Protocol)
			cerr := ap.Connect(sessID, authUser, clientIP, svc)
			s.observePhase(phaseWardenConnect, wardenStart)
			switch {
			case errors.Is(cerr, warden.ErrTooManyConns):
				log.Warn("login: warden", "user", pre.username, "result", "fail", "reason", "too_many_connections")
				// The code, then the announcement: a close is said with a BYE,
				// not by dropping the socket after the tagged NO (#928).
				writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeLimit, "too many connections")
				writeProtoClose(authConn, s.opts.Protocol, "closing")
				return outcomeClose, nil
			case cerr != nil:
				log.Error("login: warden connect failed", "addr", s.opts.WardenAddr, "err", cerr)
				if !s.opts.WardenFailOpen {
					writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "service temporarily unavailable")
					s.incResult("unavailable")
					return outcomeRetry, nil
				}
			default:
				log.Info("login: warden", "user", pre.username, "result", "ok")
				// #814: record the routed backend in warden so `who` can scope to the
				// local backend. Best-effort.
				if beIP, _, splitErr := net.SplitHostPort(backendAddr); splitErr == nil {
					if berr := ap.Backend(sessID, beIP); berr != nil {
						log.Debug("login: warden backend push", "err", berr)
					}
				}
				hbCtx, hbCancel := context.WithCancel(context.Background())
				hbDone := make(chan struct{})
				go func() {
					defer close(hbDone)
					interval := warden.DefaultSessionTTL / 3
					if err := ap.HeartbeatLoop(hbCtx, sessID, interval, nil); err != nil {
						log.Debug("login: warden heartbeat loop", "err", err)
					}
				}()
				// Only the registration is released; the pool outlives it. Held in
				// a closure so either owner can run it exactly once (#896).
				releaseWarden = func() {
					hbCancel()
					<-hbDone
					if err := ap.Disconnect(sessID, authUser, clientIP, svc); err != nil {
						log.Debug("login: warden disconnect", "err", err)
					}
				}
			}
		}

		// Dial, preamble and greeting retry as one unit: a failed greeting
		// leaves the connection unusable, and the next try may reroute (#896).
		backendDialStart := time.Now()
		var bs *backendSession
		retries := s.transientRetries()
		for attempt := 0; ; attempt++ {
			var berr error
			bs, berr = s.openBackendSession(pre, authResult, authUser, tag, backendAddr, clientIP, sessID, log)
			if berr == nil {
				break
			}
			if attempt >= retries {
				log.Error("login: backend session failed", "addr", backendAddr, "attempts", attempt+1, "err", berr)
				s.observePhase(phaseBackendDial, backendDialStart)
				s.incTransientExhausted(stageBackendSession)
				writeProtoError(authConn, s.opts.Protocol, pre.cmdTag, imapCodeUnavailable, "backend unavailable")
				s.incResult("unavailable")
				return outcomeRetry, nil
			}
			log.Warn("login: backend session failed, retrying", "addr", backendAddr, "attempt", attempt+1, "err", berr)
			s.incTransientRetry(stageBackendSession)
			time.Sleep(transientRetryBackoff)
		}
		s.observePhase(phaseBackendDial, backendDialStart)

		committed = true
		return outcomeDone, &established{bs: bs, user: authUser, releaseWarden: releaseWarden}
	}

	// A transient failure returns to the pre-auth loop on the same connection,
	// sparing a handshake; capped so a wedged backend cannot pile up (#896).
	for reloginCount := 0; ; {
		outcome, e := attempt()
		if outcome == outcomeDone {
			est = e
			break
		}
		if outcome == outcomeClose {
			return
		}
		// outcomeRetry.
		reloginCount++
		if reloginCount >= s.transientReloginCap() {
			// The budget is spent: announce the close rather than drop the
			// socket after a NO that said the connection stays (#928).
			writeProtoClose(authConn, s.opts.Protocol, "too many transient failures, closing")
			return
		}
		var retryExtTLS *tls.Config
		if _, ok := authConn.(*tls.Conn); !ok {
			retryExtTLS = s.opts.StarttlsTLS
		}
		var cerr error
		pre, authConn, authRd, cerr = continueAuth(authConn, authRd, retryExtTLS, s.opts.Protocol, s.opts, relayContext{dial: s.authClient, sessionID: sessID})
		if cerr != nil {
			log.Debug("login: transient re-login: client did not retry", "err", cerr)
			return
		}
		log.Info("login: transient re-login", "attempt", reloginCount+1)
	}

	backendConn, backendRd, backendAddr, backendCaps := est.bs.conn, est.bs.rd, est.bs.addr, est.bs.caps
	defer backendConn.Close()
	if est.releaseWarden != nil {
		defer est.releaseWarden()
	}

	// Before the session is registered, announced or counted: a declined one is
	// not up, and one session carries one result (#1776).
	if est.bs.refusal != "" {
		// Counted before the client is told: the reply is what a watcher waits
		// on, and a counter raised after it cannot be waited on (#1826).
		s.incResult("backend_declined")
		io.WriteString(authConn, est.bs.refusal) //nolint:errcheck
		log.Info("login: backend declined the session", "user", pre.username,
			"backend", backendAddr, "result", "backend_declined")
		return
	}

	// Registered once it is up, under the identity it acts as: a master session
	// filed under the typed string is invisible to every kick (#1306).
	backendIP, _, _ := net.SplitHostPort(backendAddr)
	sess := &liveSession{
		id: sessID, user: est.user, backendConn: backendConn, clientConn: authConn,
		backendIP: backendIP, proto: s.opts.Protocol.Base(),
	}
	s.announceMu.Lock()
	s.sessMu.Lock()
	s.sessions[est.user] = append(s.sessions[est.user], sess)
	s.sessMu.Unlock()
	defer func() {
		s.sessMu.Lock()
		list := s.sessions[est.user]
		for i, v := range list {
			if v == sess {
				s.sessions[est.user] = append(list[:i], list[i+1:]...)
				break
			}
		}
		s.sessMu.Unlock()
	}()

	s.announceSessionLocked(sess)
	s.announceMu.Unlock()
	// Closed through whichever watch connection is current then: a captured one
	// is dead after a reconnect, and the close would be lost (#1393).
	defer s.announceSessionClose(sessID)

	log.Info("login: session routed", "user", pre.username, "backend", backendAddr, "result", "ok")
	s.incResult("ok")

	// Auth is confirmed — tell the client before entering proxy mode.
	writeProtoAuthOK(authConn, s.opts.Protocol, pre.cmdTag, backendCaps)

	authConn.SetDeadline(time.Time{})    //nolint:errcheck
	backendConn.SetDeadline(time.Time{}) //nolint:errcheck

	proto := string(s.opts.Protocol)
	sessionsGauge.WithLabelValues(proto).Inc()
	biProxy(authRd, authConn, backendRd, backendConn, func() { backendConn.Close() })
	sessionsGauge.WithLabelValues(proto).Dec()
	log.Info("login: disconnect", "user", pre.username)
}

// directorLookup dials yarilo-director, issues a LOOKUP restricted to tag,
// and returns the backend address.
func (s *Server) directorLookup(username, tag string) (string, error) {
	id := fmt.Sprintf("%d", s.reqID.Add(1))

	// The persistent connection when there is one; otherwise dial, so a login
	// never waits on the watch being up (#878).
	s.watchMu.RLock()
	wc := s.watch
	s.watchMu.RUnlock()
	if wc != nil {
		result, lerr := wc.lookup(id, username, tag, s.opts.Protocol.Base(), directorLookupTimeout)
		if lerr == nil {
			return s.applyBackendPort(result.Addr), nil
		}
		// A dead connection or a hold is not a reason to skip the fallback path
		// for the former; a hold must propagate unchanged so the caller retries.
		if !errors.Is(lerr, errWatchClosed) {
			return "", lerr
		}
	}

	return s.directorLookupDial(id, username, tag)
}

// applyBackendPort overrides the ring port the director returns with the
// component's configured backend port.
func (s *Server) applyBackendPort(addr string) string {
	if s.opts.BackendPort <= 0 {
		return addr
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return net.JoinHostPort(host, fmt.Sprintf("%d", s.opts.BackendPort))
}

// directorLookupDial is the fallback: one connection for one lookup.
func (s *Server) directorLookupDial(id, username, tag string) (string, error) {
	var c *proto.Conn
	var err error
	if s.opts.DirectorTLS != nil {
		c, err = proto.DialTLS(s.opts.DirectorAddr, s.opts.LocalIP, 0, s.opts.DirectorTLS)
	} else {
		c, err = proto.Dial(s.opts.DirectorAddr, s.opts.LocalIP, 0)
	}
	if err != nil {
		return "", fmt.Errorf("director dial: %w", err)
	}
	defer c.Close()

	result, err := c.Lookup(id, username, tag, s.opts.Protocol.Base())
	if err != nil {
		return "", fmt.Errorf("director lookup: %w", err)
	}
	return s.applyBackendPort(result.Addr), nil
}

// The confirmed-kick retry budget: holds × backoff must exceed the director's
// worst-case confirm time, or a concurrent login errors before it (#847, #858).
const (
	defaultMaxLookupHolds    = 20
	defaultLookupHoldBackoff = 150 * time.Millisecond
)

func (s *Server) maxLookupHolds() int {
	if s.opts.LookupHoldMax > 0 {
		return s.opts.LookupHoldMax
	}
	return defaultMaxLookupHolds
}

func (s *Server) lookupHoldBackoff() time.Duration {
	if s.opts.LookupHoldBackoff > 0 {
		return s.opts.LookupHoldBackoff
	}
	return defaultLookupHoldBackoff
}

// directorLookupWithHold retries a LOOKUP held by a confirmed kick, bounded;
// anything else returns at once (#847).
func (s *Server) directorLookupWithHold(username, tag string, log *slog.Logger) (string, error) {
	maxHolds, backoff := s.maxLookupHolds(), s.lookupHoldBackoff()
	for attempt := 0; ; attempt++ {
		addr, err := s.directorLookup(username, tag)
		if err == nil || !errors.Is(err, proto.ErrLookupHold) {
			return addr, err
		}
		if attempt >= maxHolds {
			return "", err
		}
		log.Debug("login: director holding lookup (user kill in progress), retrying", "user", username, "attempt", attempt+1)
		time.Sleep(backoff)
	}
}

// maxBackendReroutes keeps the fast-fail re-route an accelerator, not a retry
// storm: a re-LOOKUP naming the same dead pod stops it anyway (#782).
const maxBackendReroutes = 1

// backendSession is a backend connection that completed the preamble handshake
// and, for submission, its EHLO exchange.
type backendSession struct {
	conn net.Conn
	rd   *bufio.Reader
	addr string
	caps string
	// refusal is the backend's own answer when it declined the session; the
	// client gets it as the reply to PASS/AUTH.
	refusal string
}

// openBackendSession brings a session up to the point the client may be told
// it succeeded; every failure closes the connection, so a caller may retry.

// backendBringupTimeout bounds preamble, greeting and EHLO: a backend silent
// that long is wedged, not slow. A var only so a test can shorten it (#927).
var backendBringupTimeout = 5 * time.Second

// resolvedIdentity is what the service resolved, or the login string when it
// named nobody -- never an empty name claimed to the backend.
func resolvedIdentity(res *authclient.AuthResult, claimed string) string {
	if res != nil && res.Username != "" {
		return res.Username
	}
	return claimed
}

func (s *Server) openBackendSession(pre *preamble, authResult *authclient.AuthResult, authUser, tag, addr, clientIP, sessID string, log *slog.Logger) (*backendSession, error) {
	// The re-route re-LOOKUPs on a failed dial, so it needs the resolved
	// identity: the typed string hashes to another pod (#782, #1306).
	conn, addr, err := s.dialBackendWithReroute(authUser, tag, addr, log)
	if err != nil {
		return nil, fmt.Errorf("dial backend %s: %w", addr, err)
	}
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()

	// One deadline over the whole bring-up: a backend that accepts TCP and never
	// greets held handlers 7-11 minutes before this (#926, #927).
	conn.SetDeadline(time.Now().Add(backendBringupTimeout)) //nolint:errcheck

	rd := bufio.NewReaderSize(conn, 4096)

	// The backend's PreambleListener reads this line and calls yarilo-auth VERIFY.
	pre2 := loginproto.Preamble{
		Addr:      clientIP,
		SessionID: sessID,
		// The identity the token was issued for: the backend's VERIFY compares
		// the two, and a name that merely resembles it fails (#1306).
		User:  authUser,
		Token: authResult.Token,
		Helo:  pre.ehloLine,
	}
	if _, werr := io.WriteString(conn, pre2.Format()); werr != nil {
		return nil, fmt.Errorf("send preamble: %w", werr)
	}

	// The greeting carries IMAP's post-auth capabilities; a backend that closes
	// here instead is reported, not dropped in silence.
	greetingStart := time.Now()
	caps, refusal, gerr := readBackendGreeting(rd, s.opts.Protocol)
	s.observePhase(phaseBackendPreamble, greetingStart)
	if gerr != nil {
		s.incResult("backend_rejected")
		return nil, fmt.Errorf("backend rejected session: %w", gerr)
	}

	// SMTP submission: send EHLO so the backend has a HELO domain before the
	// client sends MAIL FROM through the proxy.
	if isSubmission(s.opts.Protocol) {
		ehlo := pre.ehloLine
		if ehlo == "" {
			ehlo = "EHLO yarilo-submission-login\r\n"
		}
		if _, werr := io.WriteString(conn, ehlo); werr != nil {
			return nil, fmt.Errorf("smtp ehlo send: %w", werr)
		}
		for {
			line, rerr := rd.ReadString('\n')
			if rerr != nil {
				return nil, fmt.Errorf("smtp ehlo response: %w", rerr)
			}
			if len(line) >= 4 && line[3] != '-' {
				break
			}
		}
	}

	// Established: clear the bring-up deadline so the proxied session is not
	// bounded by it (handleConn clears deadlines again before proxying anyway).
	conn.SetDeadline(time.Time{}) //nolint:errcheck
	ok = true
	return &backendSession{conn: conn, rd: rd, addr: addr, caps: caps, refusal: refusal}, nil
}

// dialBackendWithReroute reports a dead backend and re-LOOKUPs once. The same
// address back means the ring has not dropped it yet, so it stops (#782).
func (s *Server) dialBackendWithReroute(username, tag, addr string, log *slog.Logger) (net.Conn, string, error) {
	conn, err := dialBackend(addr, s.opts.BackendTLS)
	if err == nil {
		return conn, addr, nil
	}
	// Standalone mode (fixed backend) has no director to re-route through.
	if s.opts.BackendAddr != "" {
		return nil, addr, err
	}
	for attempt := 0; attempt < maxBackendReroutes; attempt++ {
		log.Warn("login: backend dial failed — reporting unreachable and re-looking-up", "addr", addr, "err", err)
		s.reportUnreachable(addr)
		newAddr, lerr := s.directorLookup(username, tag)
		if lerr != nil {
			return nil, addr, fmt.Errorf("re-lookup after unreachable: %w", lerr)
		}
		if newAddr == addr {
			return nil, addr, fmt.Errorf("re-lookup returned the same unreachable backend %s", addr)
		}
		addr = newAddr
		conn, err = dialBackend(addr, s.opts.BackendTLS)
		if err == nil {
			return conn, addr, nil
		}
	}
	return nil, addr, fmt.Errorf("backend unreachable after re-route: %w", err)
}

// reportUnreachable tells the director a dial failed. Best-effort: the report
// accelerates eviction, the TTL lease is the backstop (#782).
func (s *Server) reportUnreachable(backendAddr string) {
	ip, _, err := net.SplitHostPort(backendAddr)
	if err != nil {
		ip = backendAddr
	}
	var c *proto.Conn
	if s.opts.DirectorTLS != nil {
		c, err = proto.DialTLS(s.opts.DirectorAddr, s.opts.LocalIP, 0, s.opts.DirectorTLS)
	} else {
		c, err = proto.Dial(s.opts.DirectorAddr, s.opts.LocalIP, 0)
	}
	if err != nil {
		return
	}
	defer c.Close()
	_ = c.Unreachable(ip)
}

// Watch holds the director connection that carries USER-KICKED pushes to this
// pod's sessions. One goroutine per Server; a no-op without a director (#736).
func (s *Server) Watch(ctx context.Context) {
	if s.opts.DirectorAddr == "" {
		return
	}
	backoff := 2 * time.Second
	for {
		s.runWatch(ctx)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("login: director watch disconnected, reconnecting", "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 60*time.Second {
			backoff = 60 * time.Second
		}
	}
}

func (s *Server) runWatch(ctx context.Context) {
	var c *proto.Conn
	var err error
	if s.opts.DirectorTLS != nil {
		c, err = proto.DialTLS(s.opts.DirectorAddr, s.opts.LocalIP, 0, s.opts.DirectorTLS)
	} else {
		c, err = proto.Dial(s.opts.DirectorAddr, s.opts.LocalIP, 0)
	}
	if err != nil {
		slog.Warn("login: director watch dial failed", "err", err)
		return
	}
	defer c.Close()

	wc := &watchConn{c: c}
	s.watchMu.Lock()
	s.watch = wc
	s.watchMu.Unlock()
	defer func() {
		s.watchMu.Lock()
		if s.watch == wc {
			s.watch = nil
		}
		s.watchMu.Unlock()
	}()

	slog.Info("login: director watch connected", "addr", s.opts.DirectorAddr)
	s.reannounceSessions()
	// Adding what it does not know is only half: the list below is what
	// removes what it knows and we do not.
	s.syncSessions()

	readErr := make(chan error, 1)
	go func() {
		err := s.watchReadLoop(c, wc)
		wc.failPending()
		readErr <- err
	}()

	// Reconciling while the connection holds covers a close lost any way at
	// all, which is the point of reconciling rather than patching one path.
	syncStop := make(chan struct{})
	defer close(syncStop)
	if every := s.sessionSyncInterval(); every > 0 {
		go func() {
			t := time.NewTicker(every)
			defer t.Stop()
			for {
				select {
				case <-syncStop:
					return
				case <-t.C:
					s.syncSessions()
				}
			}
		}()
	}

	select {
	case <-ctx.Done():
		c.Close()
		<-readErr
	case err := <-readErr:
		if err != nil {
			slog.Warn("login: director watch read error", "err", err)
		}
	}
}

func (s *Server) watchReadLoop(c *proto.Conn, wc *watchConn) error {
	for {
		line, err := c.ReadLine()
		if err != nil {
			return err
		}
		switch {
		case strings.HasPrefix(line, "HOST\t"), strings.HasPrefix(line, "FAIL\t"):
			// Routed by id; an unclaimed one is a reply whose caller already
			// timed out, and is dropped.
			if fields := strings.Split(line, "\t"); len(fields) >= 2 {
				wc.deliver(fields[1], line)
			}
		case strings.HasPrefix(line, "USER-KICKED\t"):
			user, ok := kickedUser(line)
			if !ok {
				// Arity checked, not trimmed: ignoring the extra fields let a ring
				// field travel into the username and kick nobody (#1363).
				slog.Warn("login: malformed USER-KICKED push, ignored", "line", line)
				break
			}
			s.kickUser(user)
		case line == "PING":
			wc.pong()
		}
		// OK and other push lines silently ignored.
	}
}

// kickedUser takes the username from a USER-KICKED push. Two forms only: the
// plain kick, and the evacuation form an older director still sends.
func kickedUser(line string) (string, bool) {
	fields := strings.Split(line, "\t")
	if len(fields) < 2 || len(fields) > 3 || fields[1] == "" {
		return "", false
	}
	return fields[1], true
}

// defaultSessionSyncInterval bounds how long a director may count a session
// nobody runs -- the thing being fixed, not the freshness of the count.
const defaultSessionSyncInterval = 30 * time.Second

// sessionSyncIDsPerLine keeps a line inside the director's 4 KiB read buffer:
// a longer one would not truncate, it would break the connection.
const sessionSyncIDsPerLine = 100

func (s *Server) sessionSyncInterval() time.Duration {
	if s.opts.SessionSyncInterval == 0 {
		return defaultSessionSyncInterval
	}
	return s.opts.SessionSyncInterval
}

// syncSessions sends the whole list: opens and closes alone leave a lost event
// wrong forever. Snapshot and write hold announceMu, or a new one is erased.
func (s *Server) syncSessions() {
	s.announceMu.Lock()
	defer s.announceMu.Unlock()

	s.watchMu.RLock()
	wc := s.watch
	s.watchMu.RUnlock()
	if wc == nil {
		return
	}

	s.sessMu.RLock()
	ids := make([]string, 0, len(s.sessions))
	for _, list := range s.sessions {
		for _, sess := range list {
			ids = append(ids, sess.id)
		}
	}
	s.sessMu.RUnlock()

	wc.sessionSyncStart()
	for i := 0; i < len(ids); i += sessionSyncIDsPerLine {
		end := i + sessionSyncIDsPerLine
		if end > len(ids) {
			end = len(ids)
		}
		wc.sessionSyncChunk(ids[i:end])
	}
	wc.sessionSyncEnd()
}

// announceSessionLocked writes SESSION-OPEN under announceMu: interleaved with
// a reconciliation, this session would be erased by an older list.
func (s *Server) announceSessionLocked(sess *liveSession) {
	s.watchMu.RLock()
	wc := s.watch
	s.watchMu.RUnlock()
	if wc == nil {
		return
	}
	wc.sessionOpen(sess.id, sess.user, sess.backendIP, sess.proto)
}

func (s *Server) announceSessionClose(sessID string) {
	s.announceMu.Lock()
	defer s.announceMu.Unlock()
	s.watchMu.RLock()
	wc := s.watch
	s.watchMu.RUnlock()
	if wc == nil {
		// Counted, not swallowed: phantoms disappearing while this stays zero
		// would mean the reconciliation covers some other path (#1393).
		metricSessionCloseDropped.Inc()
		return
	}
	wc.sessionClose(sessID)
}

// reannounceSessions re-sends SESSION-OPEN after a reconnect: the director
// that heard the first one may be gone, leaving the count low (#1393).
func (s *Server) reannounceSessions() {
	s.sessMu.RLock()
	live := make([]*liveSession, 0, len(s.sessions))
	for _, list := range s.sessions {
		live = append(live, list...)
	}
	s.sessMu.RUnlock()
	if len(live) == 0 {
		return
	}
	s.announceMu.Lock()
	for _, sess := range live {
		s.announceSessionLocked(sess)
	}
	s.announceMu.Unlock()
	slog.Info("login: re-announced live sessions to the director", "count", len(live))
}

// kickUser closes all active backend connections for the given username,
// causing biProxy to terminate and those sessions to be dropped.
func (s *Server) kickUser(username string) {
	s.sessMu.RLock()
	sessions := make([]*liveSession, len(s.sessions[username]))
	copy(sessions, s.sessions[username])
	s.sessMu.RUnlock()

	// Logged with the count, zero included: a kick that matched nothing must
	// not read like a kick that never arrived (#1363).
	slog.Info("login: user kick received", "user", username,
		"proto", string(s.opts.Protocol), "sessions", len(sessions))
	for _, sess := range sessions {
		sess.close("kicked by admin or move")
	}
}

// close tears down both legs. The reason is logged, not sent: a notice
// injected mid-literal would corrupt the stream the client is parsing.
func (sess *liveSession) close(reason string) {
	slog.Info("login: kicking session", "user", sess.user, "session", sess.id, "reason", reason)
	sess.backendConn.Close()
	if sess.clientConn != nil {
		sess.clientConn.Close()
	}
}

// kickSession closes the session with this id, whoever owns it, and says
// whether it found one: the event is broadcast and only the owner reacts.
func (s *Server) kickSession(id string) bool {
	s.sessMu.RLock()
	var target *liveSession
findLoop:
	for _, list := range s.sessions {
		for _, sess := range list {
			if sess.id == id {
				target = sess
				break findLoop
			}
		}
	}
	s.sessMu.RUnlock()
	if target == nil {
		return false
	}
	target.close("kicked by session id")
	return true
}

// kickChannel is this pod's warden channel, keyed per protocol so a binary
// wakes only for its own events. The payload is the session id.
func (s *Server) kickChannel() string {
	return "kick:" + string(s.opts.Protocol)
}

// startKickSubscriber spawns the per-protocol subscriber, or nothing when no
// warden is configured. It runs until ctx is cancelled.
func (s *Server) startKickSubscriber(ctx context.Context) {
	if s.opts.WardenAddr == "" {
		return
	}
	go s.kickSubscribeLoop(ctx, s.kickChannel())
}

// kickReconnectDelay is the backoff between kick-subscriber reconnect attempts.
const kickReconnectDelay = time.Second

// kickSubscribeLoop re-subscribes whenever the connection drops: one warden
// restart would otherwise deafen this pod to kicks for good (#908).
func (s *Server) kickSubscribeLoop(ctx context.Context, channel string) {
	for {
		if ctx.Err() != nil {
			return
		}
		ac, err := warden.Dial(s.opts.WardenAddr, s.opts.WardenTLS, 5*time.Second)
		if err != nil {
			slog.Warn("login: kick subscribe dial failed, retrying", "addr", s.opts.WardenAddr, "err", err)
			if !sleepCtx(ctx, kickReconnectDelay) {
				return
			}
			continue
		}
		ch, err := ac.Subscribe(ctx, channel)
		if err != nil {
			ac.Close()
			slog.Warn("login: kick subscribe failed, retrying", "channel", channel, "err", err)
			if !sleepCtx(ctx, kickReconnectDelay) {
				return
			}
			continue
		}
		slog.Info("login: kick subscriber connected", "channel", channel)
		for sessID := range ch {
			if !s.kickSession(sessID) {
				slog.Debug("login: kick event ignored (no match)", "session", sessID)
			}
		}
		ac.Close()
		// ch closed: either ctx cancel (exit) or a transport drop (reconnect).
		if ctx.Err() != nil {
			return
		}
		slog.Warn("login: kick subscription dropped, reconnecting", "channel", channel)
		if !sleepCtx(ctx, kickReconnectDelay) {
			return
		}
	}
}

// sleepCtx sleeps for d unless ctx is cancelled first; returns false if ctx was
// cancelled (caller should stop).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func dialBackend(addr string, tlsCfg *tls.Config) (net.Conn, error) {
	if tlsCfg != nil {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", addr, tlsCfg)
		if err != nil {
			return nil, fmt.Errorf("mtls dial %s: %w", addr, err)
		}
		return conn, nil
	}
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	return conn, nil
}

// readBackendGreeting returns IMAP's post-auth capabilities for the tagged OK,
// and the backend's own words when it declined the session (#1776).
func readBackendGreeting(rd *bufio.Reader, p Protocol) (caps, refusal string, err error) {
	switch p {
	case ProtocolIMAP, ProtocolIMAPS:
		line, err := rd.ReadString('\n')
		if err != nil {
			return "", "", err
		}
		// Extract content of [CAPABILITY ...] if present.
		if start := strings.Index(line, "[CAPABILITY "); start >= 0 {
			start += len("[CAPABILITY ")
			if end := strings.Index(line[start:], "]"); end >= 0 {
				caps = line[start : start+end]
			}
		}
		return caps, "", nil
	case ProtocolPOP3, ProtocolPOP3S:
		line, err := rd.ReadString('\n')
		if err != nil {
			return "", "", err
		}
		if strings.HasPrefix(line, "-ERR") {
			return "", line, nil
		}
		return "", "", nil
	case ProtocolSubmission, ProtocolSubmissions:
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return "", "", err
			}
			if len(line) < 4 || line[3] != '-' {
				return "", "", nil
			}
		}
	case ProtocolManageSieve:
		// Consume the backend's pre-auth greeting (capability lines + OK).
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return "", "", err
			}
			if strings.HasPrefix(line, "OK") {
				return "", "", nil
			}
		}
	}
	return "", "", nil
}

// checkAllowNets reports whether clientIP is contained in any of the comma-separated
// CIDR ranges from the allow_nets= field returned by yarilo-auth.
func checkAllowNets(clientIP, allowNets string) bool {
	ip := net.ParseIP(clientIP)
	if ip == nil {
		return false
	}
	for _, cidr := range strings.Split(allowNets, ",") {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// writeProtoAuthOK tells the client it is in, carrying the backend's post-auth
// capabilities so it skips a CAPABILITY round trip.
func writeProtoAuthOK(conn net.Conn, p Protocol, tag, caps string) {
	switch p {
	case ProtocolIMAP, ProtocolIMAPS:
		if caps != "" {
			fmt.Fprintf(conn, "%s OK [CAPABILITY %s] Logged in\r\n", tag, caps) //nolint:errcheck
		} else {
			fmt.Fprintf(conn, "%s OK Logged in\r\n", tag) //nolint:errcheck
		}
	case ProtocolPOP3, ProtocolPOP3S:
		fmt.Fprintf(conn, "+OK Logged in\r\n") //nolint:errcheck
	case ProtocolSubmission, ProtocolSubmissions:
		fmt.Fprintf(conn, "235 2.7.0 Authentication successful\r\n") //nolint:errcheck
	case ProtocolManageSieve:
		fmt.Fprintf(conn, "OK \"Logged in.\"\r\n") //nolint:errcheck
	}
}

// clientGoneGrace is how long the backend may still deliver after the client
// left: a reply in flight, never a new conversation.
const clientGoneGrace = 5 * time.Second

// biProxy returns when the session is over, the client being gone included: a
// leg with no read timeout hung, outliving the pod in every director (#1404).
func biProxy(clientRd io.Reader, clientW io.Writer, backendRd io.Reader, backendW io.Writer, closeBackend func()) {
	backendDone := make(chan struct{})
	go func() {
		defer close(backendDone)
		io.Copy(clientW, backendRd) //nolint:errcheck
		halfClose(clientW)
	}()

	io.Copy(backendW, clientRd) //nolint:errcheck
	halfClose(backendW)

	// The client is gone: wait briefly for what the backend still owes, then
	// take the leg down rather than hold a conversation nobody can continue.
	select {
	case <-backendDone:
		return
	case <-time.After(clientGoneGrace):
	}
	if closeBackend != nil {
		closeBackend()
	}
	<-backendDone
}

func halfClose(w io.Writer) {
	type halfCloser interface{ CloseWrite() error }
	if hc, ok := w.(halfCloser); ok {
		hc.CloseWrite() //nolint:errcheck
	}
}

// IMAP response codes (RFC 5530) used in NO responses.
const (
	imapCodeUnavailable        = "UNAVAILABLE"
	imapCodeAuthenticationFail = "AUTHENTICATIONFAILED"
	imapCodeLimit              = "LIMIT"
)

func writeProtoError(conn net.Conn, p Protocol, tag, imapCode, msg string) {
	switch p {
	case ProtocolIMAP, ProtocolIMAPS:
		if tag != "" {
			fmt.Fprintf(conn, "%s NO [%s] %s\r\n", tag, imapCode, msg) //nolint:errcheck
		} else {
			fmt.Fprintf(conn, "* BYE %s\r\n", msg) //nolint:errcheck
		}
	case ProtocolPOP3, ProtocolPOP3S:
		// RFC 3206: map error class to [AUTH] / [SYS/TEMP] response codes.
		switch imapCode {
		case imapCodeAuthenticationFail:
			fmt.Fprintf(conn, "-ERR [AUTH] %s\r\n", msg) //nolint:errcheck
		case imapCodeUnavailable:
			fmt.Fprintf(conn, "-ERR [SYS/TEMP] %s\r\n", msg) //nolint:errcheck
		default:
			fmt.Fprintf(conn, "-ERR %s\r\n", msg) //nolint:errcheck
		}
	case ProtocolSubmission, ProtocolSubmissions:
		// A transient failure keeps the connection, so it must not say 421,
		// which means "closing" and makes a compliant client hang up (#896).
		switch imapCode {
		case imapCodeUnavailable:
			fmt.Fprintf(conn, "454 4.7.0 %s\r\n", msg) //nolint:errcheck
		default:
			fmt.Fprintf(conn, "421 4.3.0 %s\r\n", msg) //nolint:errcheck
		}
	case ProtocolManageSieve:
		// BYE announces a close too, so a transient failure answers NO instead:
		// a client that hears BYE hangs up, defeating the keep-open (#896).
		switch imapCode {
		case imapCodeAuthenticationFail:
			fmt.Fprintf(conn, "NO (AUTHENTICATIONFAILED) %q\r\n", msg) //nolint:errcheck
		case imapCodeUnavailable:
			fmt.Fprintf(conn, "NO (TRYLATER) %q\r\n", msg) //nolint:errcheck
		default:
			fmt.Fprintf(conn, "NO %q\r\n", msg) //nolint:errcheck
		}
	}
}

// writeProtoClose announces a close; writeProtoError keeps the connection. The
// two must stay apart, or a retry is told goodbye and a close is silent (#928).
func writeProtoClose(conn net.Conn, p Protocol, msg string) {
	switch p {
	case ProtocolIMAP, ProtocolIMAPS:
		fmt.Fprintf(conn, "* BYE %s\r\n", msg) //nolint:errcheck
	case ProtocolPOP3, ProtocolPOP3S:
		fmt.Fprintf(conn, "-ERR [SYS/TEMP] %s\r\n", msg) //nolint:errcheck
	case ProtocolSubmission, ProtocolSubmissions:
		fmt.Fprintf(conn, "421 4.3.0 %s\r\n", msg) //nolint:errcheck
	case ProtocolManageSieve:
		fmt.Fprintf(conn, "BYE %q\r\n", msg) //nolint:errcheck
	}
}

func isSubmission(p Protocol) bool {
	return p == ProtocolSubmission || p == ProtocolSubmissions
}

// isRetriableProtocol reports whether the protocol keeps the connection open
// after a failed authentication attempt (IMAP and POP3 do; SMTP closes).
func isRetriableProtocol(p Protocol) bool {
	return p == ProtocolIMAP || p == ProtocolIMAPS ||
		p == ProtocolPOP3 || p == ProtocolPOP3S ||
		p == ProtocolManageSieve
}

// wardenService maps a login Protocol to the service name used in the warden protocol.
func wardenService(p Protocol) string {
	switch p {
	case ProtocolPOP3, ProtocolPOP3S:
		return "pop3"
	case ProtocolSubmission, ProtocolSubmissions:
		return "smtp"
	case ProtocolManageSieve:
		return "managesieve"
	default:
		return "imap"
	}
}

// ipInNets gates XCLIENT/ID forwarding on the trusted nets; empty trusts
// nobody (#742).
func ipInNets(ip string, nets []*net.IPNet) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

func haProxyPolicy(nets []*net.IPNet) func(net.Addr) (proxyproto.Policy, error) {
	return func(upstream net.Addr) (proxyproto.Policy, error) {
		if len(nets) == 0 {
			return proxyproto.IGNORE, nil
		}
		tcpAddr, ok := upstream.(*net.TCPAddr)
		if !ok {
			return proxyproto.IGNORE, nil
		}
		for _, n := range nets {
			if n.Contains(tcpAddr.IP) {
				return proxyproto.USE, nil
			}
		}
		return proxyproto.IGNORE, nil
	}
}
