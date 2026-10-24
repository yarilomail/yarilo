package jmap

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/yarilomail/yarilo/internal/userstate/counters"
	"github.com/yarilomail/yarilo/pkg/jmapcore"
)

// wsSubprotocol is the one RFC 8887 §4.2 registers.
const wsSubprotocol = "jmap"

// wsConn is one JMAP WebSocket: requests answered in order, and at most one
// push subscription beside them.
type wsConn struct {
	s  *Server
	c  *websocket.Conn
	id identity

	mu         sync.Mutex
	stopPush   context.CancelFunc
	pushExited chan struct{}
}

// offersJMAP reports whether the client asked for the jmap subprotocol.
func offersJMAP(r *http.Request) bool {
	for _, h := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(h, ",") {
			if strings.TrimSpace(p) == wsSubprotocol {
				return true
			}
		}
	}
	return false
}

// handleWebSocket serves JMAP over WebSocket (RFC 8887).
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request, id identity) {
	// Counted before the upgrade: until the hijack Shutdown waits for the
	// request, after it only this counter does.
	s.websockets.Add(1)
	defer s.websockets.Done()
	if s.opts.WebSocketTimeout <= 0 {
		http.NotFound(w, r)
		return
	}
	if !offersJMAP(r) {
		jmapcore.WriteProblem(w, http.StatusBadRequest, "The jmap WebSocket subprotocol is required")
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{wsSubprotocol},
		// The login proxy enforces the allowed browser origins.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionNoContextTakeover,
	})
	if err != nil {
		slog.Debug("jmap: websocket refused", "user", id.user, "err", err)
		return
	}
	// The size check is ours, so an oversized request gets a RequestError.
	c.SetReadLimit(-1)
	metricWebSockets.Inc()
	defer metricWebSockets.Dec()
	slog.Debug("jmap: websocket open", "user", id.user, "session", id.sessionID)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	ws := &wsConn{s: s, c: c, id: id}
	defer ws.disablePush()
	go ws.keepAlive(ctx, s.opts.WebSocketTimeout)
	go func() {
		select {
		case <-s.done:
			_ = c.Close(websocket.StatusGoingAway, "server shutting down")
		case <-ctx.Done():
		}
	}()

	for {
		typ, rd, err := c.Reader(ctx)
		if err != nil {
			slog.Debug("jmap: websocket closed", "user", id.user, "session", id.sessionID, "err", err)
			return
		}
		if typ != websocket.MessageText {
			_ = c.Close(websocket.StatusUnsupportedData, "text frames only")
			return
		}
		body, err := io.ReadAll(io.LimitReader(rd, s.opts.Limits.MaxSizeRequest+1))
		if err != nil {
			return
		}
		if int64(len(body)) > s.opts.Limits.MaxSizeRequest {
			if _, err := io.Copy(io.Discard, rd); err != nil {
				return
			}
			ws.send(ctx, requestError(nil, &jmapcore.RequestError{
				Type: jmapcore.ProblemLimit, Status: http.StatusRequestEntityTooLarge,
				Limit: "maxSizeRequest", Detail: "The request exceeds maxSizeRequest.",
			}))
			continue
		}
		ws.message(ctx, body)
	}
}

// keepAlive pings at half the timeout and closes a peer that does not answer.
func (ws *wsConn) keepAlive(ctx context.Context, timeout time.Duration) {
	interval := timeout / 2
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, interval)
		err := ws.c.Ping(pctx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				slog.Debug("jmap: websocket peer silent, closing", "user", ws.id.user, "err", err)
			}
			_ = ws.c.CloseNow()
			return
		}
	}
}

// wsMessage is the part of every client message that says what it is.
type wsMessage struct {
	Type      string          `json:"@type"`
	ID        json.RawMessage `json:"id"`
	DataTypes []string        `json:"dataTypes"`
	PushState string          `json:"pushState"`
}

func (ws *wsConn) message(ctx context.Context, body []byte) {
	var m wsMessage
	if err := json.Unmarshal(body, &m); err != nil {
		ws.send(ctx, requestError(nil, &jmapcore.RequestError{
			Type: jmapcore.ProblemNotJSON, Status: http.StatusBadRequest, Detail: "The message is not valid JSON.",
		}))
		return
	}
	push := ws.s.push != nil
	switch {
	case m.Type == "Request":
		resp, rerr := ws.s.runRequest(ctx, jmapcore.WebSocketPath, body, ws.id)
		if rerr != nil {
			ws.send(ctx, requestError(m.ID, rerr))
			return
		}
		ws.send(ctx, response(m.ID, resp))
	case m.Type == "WebSocketPushEnable" && push:
		ws.enablePush(m.DataTypes, m.PushState)
	case m.Type == "WebSocketPushDisable" && push:
		ws.disablePush()
	default:
		ws.send(ctx, requestError(m.ID, &jmapcore.RequestError{
			Type: jmapcore.ProblemNotRequest, Status: http.StatusBadRequest,
			Detail: "Unknown message @type: " + m.Type,
		}))
	}
}

func (ws *wsConn) send(ctx context.Context, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		slog.Warn("jmap: websocket encode", "user", ws.id.user, "err", err)
		return
	}
	if err := ws.c.Write(ctx, websocket.MessageText, b); err != nil {
		slog.Debug("jmap: websocket write", "user", ws.id.user, "err", err)
	}
}

// response is the RFC 8887 §4.3.2 Response: the API envelope tagged with
// the request's id.
func response(id json.RawMessage, resp *jmapcore.Response) map[string]any {
	out := map[string]any{
		"@type":           "Response",
		"methodResponses": resp.MethodResponses,
		"sessionState":    resp.SessionState,
	}
	if resp.CreatedIDs != nil {
		out["createdIds"] = *resp.CreatedIDs
	}
	if len(id) > 0 {
		out["requestId"] = id
	}
	return out
}

// requestError is the RFC 8887 §4.3.4 RequestError.
func requestError(id json.RawMessage, e *jmapcore.RequestError) map[string]any {
	out := map[string]any{"@type": "RequestError", "type": e.Type, "status": e.Status, "detail": e.Detail}
	if e.Limit != "" {
		out["limit"] = e.Limit
	}
	if len(id) > 0 {
		out["requestId"] = id
	}
	return out
}

// enablePush replaces any earlier subscription (RFC 8887 §4.3.5). It returns
// once the subscription has its base, so a write the client sees answered
// after the enable is never folded into that base unsent.
func (ws *wsConn) enablePush(dataTypes []string, pushState string) {
	ws.disablePush()
	types := ws.s.streamTypes("*")
	if dataTypes != nil {
		types = ws.s.streamTypes(strings.Join(dataTypes, ","))
	}
	if len(types) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	started := make(chan struct{})
	ws.mu.Lock()
	ws.stopPush, ws.pushExited = cancel, exited
	ws.mu.Unlock()
	go func() {
		defer close(exited)
		ws.pushLoop(ctx, types, pushState, started)
	}()
	select {
	case <-started:
	case <-exited:
	}
}

func (ws *wsConn) disablePush() {
	ws.mu.Lock()
	cancel, exited := ws.stopPush, ws.pushExited
	ws.stopPush, ws.pushExited = nil, nil
	ws.mu.Unlock()
	if cancel != nil {
		cancel()
		<-exited
	}
}

// beforePushBase lets a test widen the window before a subscription's base.
var beforePushBase = func() {}

// pushLoop sends a StateChange whenever a subscribed type moves. Given a
// pushState, the types that moved since it are sent at once.
func (ws *wsConn) pushLoop(ctx context.Context, types []counters.Type, pushState string, started chan<- struct{}) {
	s, user := ws.s, ws.id.user
	wt, notify := s.push.join(user)
	defer s.push.leave(wt, notify)
	select {
	case <-wt.ready:
	case <-ctx.Done():
		return
	case <-s.push.done:
		return
	}
	beforePushBase()
	sent, resumed := decodePushState(pushState)
	if !resumed {
		base, err := s.statesOf(ctx, user, types)
		if err != nil {
			slog.Warn("jmap: push base state", "user", user, "err", err)
		}
		sent = base
		if sent == nil {
			sent = map[counters.Type]string{}
		}
	}
	close(started)
	moved := []counters.Type(nil)
	if resumed {
		moved = types
	}
	for {
		if moved == nil {
			select {
			case <-ctx.Done():
				return
			case <-s.push.done:
				return
			case moved = <-notify:
			}
		}
		out := s.newStates(ctx, user, intersect(moved, types), sent)
		moved = nil
		if len(out) == 0 {
			continue
		}
		ws.send(ctx, map[string]any{
			"@type":     "StateChange",
			"changed":   map[string]any{jmapcore.AccountID(user): out},
			"pushState": encodePushState(sent),
		})
		metricPushEvents.WithLabelValues("state").Inc()
	}
}

// encodePushState carries every state sent so far, so a client resuming with
// it is sent exactly the types that moved since; the server keeps nothing.
func encodePushState(sent map[counters.Type]string) string {
	b, err := json.Marshal(sent)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodePushState(v string) (map[counters.Type]string, bool) {
	if v == "" {
		return nil, false
	}
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return nil, false
	}
	var m map[counters.Type]string
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}
