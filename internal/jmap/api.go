package jmap

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/yarilomail/yarilo/v2/pkg/jmapcore"
)

// apiPath is the endpoint the session resource advertises as apiUrl.
const apiPath = "/jmap/api/"

// declaredCapabilities is what this server implements. It is the set a client's
// "using" is checked against, and it is passed to jmapcore rather than read
// there.
func declaredCapabilities(lim jmapcore.Limits) []string {
	caps := []string{jmapcore.CapCore, jmapcore.CapMail, jmapcore.CapQuota}
	if lim.Sieve != nil {
		caps = append(caps, jmapcore.CapSieve)
	}
	if lim.Submission != nil {
		caps = append(caps, jmapcore.CapSubmission)
	}
	if lim.Vacation != nil {
		caps = append(caps, jmapcore.CapVacation)
	}
	if lim.WebSocket != nil {
		caps = append(caps, jmapcore.CapWebSocket)
	}
	return caps
}

// registry is the method set for one request. It is built per request because
// the data methods close over that user's storage handle; JMAP has no session
// to hang them on.
func (s *Server) registry(lazy *lazyStore, accountID string) jmapcore.Registry {
	reg := jmapcore.CoreRegistry()
	if lazy.storage != nil {
		for name, entry := range s.mailboxRegistry(lazy, accountID) {
			reg[name] = entry
		}
		for name, entry := range s.quotaRegistry(lazy, accountID) {
			reg[name] = entry
		}
		if s.opts.Sieve != nil {
			for name, entry := range s.sieveRegistry(lazy, accountID) {
				reg[name] = entry
			}
			if s.opts.Sieve.Vacation {
				for name, entry := range s.vacationRegistry(lazy, accountID) {
					reg[name] = entry
				}
			}
		}
		if s.opts.Submission != nil {
			for name, entry := range s.submissionRegistry(lazy, accountID) {
				reg[name] = entry
			}
		}
	}
	return reg
}

// handleAPI runs one batch of method calls (RFC 8620 §3).
func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request, id identity) {
	// The login layer caps the body at jmap_max_size_request before proxying,
	// so this is the backend's own floor rather than the primary check: a
	// request reaching it oversized means the hop was bypassed.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.opts.Limits.MaxSizeRequest))
	if err != nil {
		slog.Warn("jmap: request body rejected", "user", id.user, "session", id.sessionID, "err", err)
		(&jmapcore.RequestError{
			Type:   jmapcore.ProblemLimit,
			Status: http.StatusRequestEntityTooLarge,
			Limit:  "maxSizeRequest",
			Detail: "The request body exceeds maxSizeRequest.",
		}).Write(w)
		return
	}
	resp, rerr := s.runRequest(r.Context(), r.URL.Path, body, id)
	if rerr != nil {
		rerr.Write(w)
		return
	}
	jmapcore.WriteJSON(w, http.StatusOK, resp)
}

// runRequest parses and runs one API request, over HTTP or a WebSocket.
func (s *Server) runRequest(ctx context.Context, path string, body []byte, id identity) (*jmapcore.Response, *jmapcore.RequestError) {
	req, rerr := jmapcore.ParseRequest(body, declaredCapabilities(s.opts.Limits), s.opts.Limits)
	if rerr != nil {
		slog.Debug("jmap: request rejected", "user", id.user, "session", id.sessionID, "path", path,
			"type", rerr.Type, "detail", rerr.Detail, "request", requestOutline(body))
		return nil, rerr
	}
	slog.Debug("jmap: request", "user", id.user, "session", id.sessionID, "path", path, "request", requestOutline(body))
	// The store opens lazily, on the first method that actually needs it, and
	// closes with the request. Opening up front would fail a whole batch on a
	// dependency the batch may not use — and Core/echo, which exists to
	// diagnose exactly that, would be the first casualty.
	lazy := &lazyStore{storage: s.opts.Storage, user: id.user, sessionID: id.sessionID, clientIP: id.clientIP}
	defer lazy.close()

	resp := jmapcore.Execute(ctx, req, s.registry(lazy, jmapcore.AccountID(id.user)), s.opts.Limits)
	return resp, nil
}

// requestOutline is what a request asks for: its capabilities and method
// names, never arguments. It reads a request ParseRequest refused as well,
// and only when the line is written.
type requestOutline []byte

func (o requestOutline) LogValue() slog.Value {
	var r struct {
		Using       []string            `json:"using"`
		MethodCalls [][]json.RawMessage `json:"methodCalls"`
	}
	_ = json.Unmarshal(o, &r)
	var methods []string
	for _, call := range r.MethodCalls {
		var name string
		if len(call) > 0 && json.Unmarshal(call[0], &name) == nil {
			methods = append(methods, name)
		}
	}
	return slog.GroupValue(slog.Any("using", r.Using), slog.Any("methods", methods))
}
