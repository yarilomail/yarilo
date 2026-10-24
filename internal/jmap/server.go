// Package jmap is the JMAP backend: the pod that owns one user's state and
// answers the JMAP methods for it. It performs no authentication of its own —
// yarilo-jmap-login has already run the passdb chain and asserts the user in a
// request header — so everything here rests on the trust gate in trust.go.
//
// The protocol layer itself lives in pkg/jmapcore, which knows nothing about
// yarilo.
package jmap

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/pkg/jmapcore"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
	"github.com/yarilomail/yarilo/pkg/quota"
)

// Options wires the backend.
type Options struct {
	Addr string
	// TLSConfig is the internal mTLS server config. Nil serves plain HTTP,
	// which only the trusted-nets mode can make safe.
	TLSConfig *tls.Config
	// Trust decides which peers may speak for a user.
	Trust Trust
	// Limits are the bounds advertised in the session resource.
	Limits jmapcore.Limits
	// OnListen fires once the port is bound. The co-located pod publishes its
	// readiness from it, so it must not run before the listener exists.
	OnListen func()
	// MaxBodyValueBytes is the server's ceiling on a returned body value. The
	// client's own maxBodyValueBytes is honoured when smaller; a client naming
	// none gets this, since the ceiling bounds the operator's work.
	MaxBodyValueBytes uint32
	// QueryMaxLimit caps how many ids one Foo/query returns. The client's own
	// limit wins when smaller; a client naming none gets this rather than
	// everything, and the response reports the limit that was applied.
	QueryMaxLimit uint
	// SnippetMaxChars bounds the preview a SearchSnippet carries. The subject
	// is not cut: it is short already, and a truncated one reads as damaged.
	SnippetMaxChars int
	// FTS answers the full-text conditions of Email/query. Nil leaves the
	// index-only evaluator, which refuses those conditions by name.
	FTS *FTS
	// Storage reaches one user's mail. Nil leaves only the methods that need no
	// mail store, which is what the session-resource-only tests run against.
	Storage *Storage
	// QuotaPolicy is the site-wide quota tunables, the same ones IMAP answers
	// GETQUOTA from: two accountings would be two answers to one question.
	QuotaPolicy quota.Policy
	// QuotaName is the root name clients see; empty takes the shared default.
	QuotaName string
	// UploadTTL is how long an uploaded blob lives (jmap_upload_ttl).
	UploadTTL time.Duration
	// QuotaEnabled gates the Quota objects: with the engine off the limits in
	// userdb are not enforced, and reporting them would be a limit that is not.
	QuotaEnabled bool
	// Sieve serves RFC 9661; nil leaves it unadvertised.
	Sieve *SieveScripts
	// Submission serves Identity and EmailSubmission; nil, as with no relay,
	// leaves them unadvertised.
	Submission *Submission
	// PushPoll is the safety recompute of a push stream's states, for events
	// lost while the lock service's channel was down; 0 turns push off.
	PushPoll time.Duration
	// WebSocketTimeout closes a WebSocket that answers nothing for this long;
	// 0 turns WebSocket off.
	WebSocketTimeout time.Duration
}

// Server serves the JMAP endpoint behind the login proxy.
type Server struct {
	opts Options
	mux  *http.ServeMux
	// states caches the per-folder markers the Email state is built from.
	// Shared across requests on purpose: the handle is not, which is why the
	// walk was paid in full on every poll (#1343).
	states *stateCache
	// push serves EventSource streams; nil when push is off.
	push *pushHub
	// done closes every WebSocket when the server shuts down.
	done     chan struct{}
	stopOnce sync.Once
	// websockets counts the WebSocket handlers still running: a hijacked
	// connection is outside what http.Server.Shutdown waits for.
	websockets sync.WaitGroup
}

// New builds the server and registers its routes.
func New(opts Options) *Server {
	// Memoise the per-driver backend once. JMAP has no session, so a store is
	// built per request; without this each request's per-user handle builds a
	// fresh backend and its own write semaphore (#1149). Storage is a pointer,
	// so this mutates the caller's struct in place (unlike the copy-of-opts wrap
	// on the other surfaces) -- deliberate: New runs once and the caller does not
	// reuse the Storage, and a second New would only re-wrap an already-memoised
	// accessor, a harmless no-op.
	if opts.Storage != nil {
		opts.Storage.MailboxByDriver = mailbox.MemoizeByDriver(opts.Storage.MailboxByDriver)
	}
	if opts.Sieve != nil {
		opts.Limits.Sieve = opts.Sieve.session()
		if opts.Sieve.Vacation {
			opts.Limits.Vacation = &jmapcore.VacationCapability{}
		}
	}
	if opts.Submission != nil {
		opts.Limits.Submission = opts.Submission.capability()
	}
	if opts.WebSocketTimeout > 0 {
		opts.Limits.WebSocket = &jmapcore.WebSocketCapability{SupportsPush: opts.PushPoll > 0}
	}
	s := &Server{opts: opts, mux: http.NewServeMux(), states: newStateCache(), done: make(chan struct{})}
	if opts.PushPoll > 0 {
		s.push = newPushHub(s, opts.PushPoll)
	}
	s.mux.HandleFunc("GET "+eventSourcePath, s.guard(s.handleEventSource))
	s.mux.HandleFunc("GET "+jmapcore.WebSocketPath, s.guard(s.handleWebSocket))
	s.mux.HandleFunc("GET "+jmapcore.SessionPath, s.guard(s.handleSession))
	s.mux.HandleFunc("POST "+apiPath, s.guard(s.handleAPI))
	s.mux.HandleFunc("GET "+downloadPrefix, s.guard(s.handleDownload))
	s.mux.HandleFunc("POST "+jmapcore.UploadPath, s.guard(s.handleUpload))
	return s
}

// Handler exposes the routes for tests without binding a port.
func (s *Server) Handler() http.Handler { return s.mux }

// Serve runs until ctx is cancelled, then drains in-flight requests.
func (s *Server) Serve(ctx context.Context) error {
	// An unanchored backend keeps its port bound and answers 403 with a named
	// cause. A dead port reads as a network fault and misdirects the operator.
	if s.opts.Trust.Mode == TrustNone {
		slog.Error("jmap: no trust anchor for the login hop — set internal_tls or xclient.trusted_nets; every request will be refused")
	}
	srv := &http.Server{
		Addr:              s.opts.Addr,
		Handler:           s.mux,
		TLSConfig:         s.opts.TLSConfig,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("jmap: listen %s: %w", s.opts.Addr, err)
	}
	if s.opts.OnListen != nil {
		s.opts.OnListen()
	}
	errCh := make(chan error, 1)
	go func() {
		if s.opts.TLSConfig != nil {
			errCh <- srv.ServeTLS(ln, "", "")
			return
		}
		errCh <- srv.Serve(ln)
	}()
	select {
	case <-ctx.Done():
		// Streams never finish on their own; Shutdown would wait them out.
		if s.push != nil {
			close(s.push.done)
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := srv.Shutdown(shutdownCtx)
		s.stopWebSockets(shutdownCtx)
		return err
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// stopWebSockets closes every WebSocket and waits for their handlers, so none
// touches a user's store after the server has stopped.
func (s *Server) stopWebSockets(ctx context.Context) {
	s.stopOnce.Do(func() { close(s.done) })
	waited := make(chan struct{})
	go func() { s.websockets.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-ctx.Done():
	}
}

// handlerFunc is a route that has already been given a resolved identity.
type handlerFunc func(http.ResponseWriter, *http.Request, identity)

// guard runs the trust gate and the contract headers before any route. Every
// identity-bearing route goes through it; there is no path that reads
// X-Yarilo-User without it.
func (s *Server) guard(next handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.opts.Trust.allows(r) {
			slog.Warn("jmap: identity refused from untrusted peer",
				"peer", hostOnly(r.RemoteAddr), "mode", s.opts.Trust.Mode.String())
			jmapcore.WriteProblem(w, http.StatusForbidden, "Untrusted peer")
			return
		}
		id, err := readIdentity(r)
		if err != nil {
			slog.Warn("jmap: bad forwarded identity", "peer", hostOnly(r.RemoteAddr), "err", err)
			jmapcore.WriteProblem(w, http.StatusForbidden, "Invalid forwarded identity")
			return
		}
		// A budget already at zero means the request has been round-tripping
		// between proxies; answering it would extend the loop.
		if id.ttl == 0 {
			slog.Warn("jmap: proxy ttl exhausted", "user", id.user, "session", id.sessionID)
			jmapcore.WriteProblem(w, http.StatusLoopDetected, "Proxy TTL exhausted")
			return
		}
		// The lock owner rides the context, as managesieve and delivery set it:
		// a store below the method has no other way to learn it (#1670).
		next(w, r.WithContext(locks.WithID(r.Context(), id.sessionID)), id)
	}
}

// handleSession answers the session resource (RFC 8620 §2.2).
func (s *Server) handleSession(w http.ResponseWriter, _ *http.Request, id identity) {
	slog.Debug("jmap: session", "user", id.user, "session", id.sessionID, "client_ip", id.clientIP)
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	jmapcore.WriteJSON(w, http.StatusOK, jmapcore.BuildSession(s.opts.Limits, id.user))
}
