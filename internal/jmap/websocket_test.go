package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/v2/pkg/jmapcore"
)

// hour keeps the safety recompute out of the way: only events move a state.
const hour = time.Hour

// dialWS opens a JMAP WebSocket to the fixture as testUser.
// dialWS returns the connection, or the status the upgrade was refused with.
func (f *pushFixture) dialWS(t *testing.T, protocols ...string) (*websocket.Conn, int, error) {
	t.Helper()
	if protocols == nil {
		protocols = []string{wsSubprotocol}
	}
	h := http.Header{}
	h.Set(hdrUser, testUser)
	h.Set(hdrSessionID, "deadbeefdeadbeef")
	h.Set(hdrProxyTTL, "4")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.ts.URL, "http")+jmapcore.WebSocketPath, //nolint:bodyclose // the library owns the handshake response and nils its body
		&websocket.DialOptions{HTTPHeader: h, Subprotocols: protocols})
	if c != nil {
		t.Cleanup(func() { _ = c.CloseNow() })
	}
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	return c, status, err
}

func (f *pushFixture) mustWS(t *testing.T) *websocket.Conn {
	t.Helper()
	c, _, err := f.dialWS(t)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func wsSend(t *testing.T, c *websocket.Conn, msg string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		t.Fatal(err)
	}
}

// wsNext reads the next message of the given @type, skipping others.
func wsNext(t *testing.T, c *websocket.Conn, typ string, within time.Duration) (map[string]any, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			return nil, false
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("message %q: %v", b, err)
		}
		if m["@type"] == typ {
			return m, true
		}
	}
}

func wsChanged(m map[string]any) map[string]any {
	c, _ := m["changed"].(map[string]any)
	out, _ := c[testAccount].(map[string]any)
	return out
}

// enable subscribes and waits for an answer to a later request: messages are
// handled in order, so the subscription is in place once it comes.
func enable(t *testing.T, c *websocket.Conn, msg string) {
	t.Helper()
	wsSend(t, c, msg)
	wsSend(t, c, echoRequest)
	if _, ok := wsNext(t, c, "Response", time.Second); !ok {
		t.Fatal("no Response after the enable")
	}
}

const echoRequest = `{"@type":"Request","id":"r1","using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{"x":1},"c0"]]}`

func TestTheSessionAdvertisesWebSocket(t *testing.T) {
	for _, tc := range []struct {
		name     string
		timeout  time.Duration
		poll     time.Duration
		want     bool
		wantPush bool
	}{
		{"on with push", time.Minute, hour, true, true},
		{"on without push", time.Minute, 0, true, false},
		{"off", 0, hour, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Options{Limits: testLimits(), PushPoll: tc.poll, WebSocketTimeout: tc.timeout})
			sess := jmapcore.BuildSession(s.opts.Limits, testUser)
			got, ok := sess.Capabilities[jmapcore.CapWebSocket].(jmapcore.WebSocketCapability)
			if ok != tc.want {
				t.Fatalf("capability present %v, want %v", ok, tc.want)
			}
			if !ok {
				return
			}
			if got.URL != "wss://mail.example.com/jmap/ws/" || got.SupportsPush != tc.wantPush {
				t.Errorf("capability %+v, want wss://mail.example.com/jmap/ws/ with supportsPush %v", got, tc.wantPush)
			}
		})
	}
}

// Without the jmap subprotocol, or with WebSocket off, there is no upgrade.
func TestAWebSocketNeedsTheSubprotocol(t *testing.T) {
	for _, tc := range []struct {
		name      string
		timeout   time.Duration
		protocols []string
		status    int
	}{
		{"no subprotocol", time.Minute, []string{"chat"}, http.StatusBadRequest},
		{"websocket off", 0, nil, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := pushServer(t, hour, 0, func(o *Options) { o.WebSocketTimeout = tc.timeout })
			_, status, err := f.dialWS(t, tc.protocols...)
			if err == nil {
				t.Fatal("upgraded")
			}
			if status != tc.status {
				t.Fatalf("status %d, want %d", status, tc.status)
			}
		})
	}
}

// A Request is answered by the API's own code, tagged with the request's id.
func TestAWebSocketRequestIsAnswered(t *testing.T) {
	f := pushServer(t, hour, 0)
	c := f.mustWS(t)
	if c.Subprotocol() != wsSubprotocol {
		t.Fatalf("subprotocol %q, want jmap", c.Subprotocol())
	}
	wsSend(t, c, echoRequest)
	m, ok := wsNext(t, c, "Response", time.Second)
	if !ok {
		t.Fatal("no Response")
	}
	if m["requestId"] != "r1" || m["sessionState"] != jmapcore.SessionState(f.s.opts.Limits) {
		t.Errorf("response %v, want requestId r1 and the session state", m)
	}
	calls, _ := m["methodResponses"].([]any)
	first, _ := calls[0].([]any)
	if len(calls) != 1 || first[0] != "Core/echo" {
		t.Errorf("methodResponses %v, want the echo", calls)
	}
}

func TestAWebSocketRefusesWithARequestError(t *testing.T) {
	big := `{"@type":"Request","id":"big","using":[],"methodCalls":[],"pad":"` + strings.Repeat("x", 2048) + `"}`
	for _, tc := range []struct {
		name, msg, typ, limit string
		id                    any
	}{
		{"not JSON", "{", jmapcore.ProblemNotJSON, "", nil},
		{"an unknown @type", `{"@type":"Ping","id":"p"}`, jmapcore.ProblemNotRequest, "", "p"},
		{"a request without calls", `{"@type":"Request","id":"n","using":[]}`, jmapcore.ProblemNotRequest, "", "n"},
		{"a request over maxSizeRequest", big, jmapcore.ProblemLimit, "maxSizeRequest", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := pushServer(t, hour, 0, func(o *Options) { o.Limits.MaxSizeRequest = 1024 })
			c := f.mustWS(t)
			wsSend(t, c, tc.msg)
			m, ok := wsNext(t, c, "RequestError", time.Second)
			if !ok {
				t.Fatal("no RequestError")
			}
			if m["type"] != tc.typ || m["requestId"] != tc.id || (tc.limit != "" && m["limit"] != tc.limit) {
				t.Errorf("RequestError %v, want type %s, requestId %v, limit %q", m, tc.typ, tc.id, tc.limit)
			}
			// The connection carries on after a refusal.
			wsSend(t, c, echoRequest)
			if _, ok := wsNext(t, c, "Response", time.Second); !ok {
				t.Error("no Response after the refusal")
			}
		})
	}
}

func TestAWebSocketClosesOnABinaryFrame(t *testing.T) {
	f := pushServer(t, hour, 0)
	c := f.mustWS(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageBinary, []byte(echoRequest)); err != nil {
		t.Fatal(err)
	}
	_, _, err := c.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusUnsupportedData {
		t.Errorf("close status %v (%v), want unsupported data", got, err)
	}
}

// A flag set elsewhere reaches an enabled push as a StateChange; after a
// disable nothing more comes.
func TestWebSocketPushCarriesAStateChange(t *testing.T) {
	f := pushServer(t, hour, 0)
	c := f.mustWS(t)
	enable(t, c, `{"@type":"WebSocketPushEnable","dataTypes":["Email"]}`)
	f.flag(t, "$seen")
	m, ok := wsNext(t, c, "StateChange", eventWait)
	if !ok {
		t.Fatal("no StateChange after the flag")
	}
	if wsChanged(m)["Email"] == nil || m["pushState"] == "" {
		t.Errorf("StateChange %v, want Email and a pushState", m)
	}
	wsSend(t, c, `{"@type":"WebSocketPushDisable"}`)
	wsSend(t, c, echoRequest)
	if _, ok := wsNext(t, c, "Response", time.Second); !ok {
		t.Fatal("no Response after the disable")
	}
	f.flag(t, "$flagged")
	if m, ok := wsNext(t, c, "StateChange", 300*time.Millisecond); ok {
		t.Errorf("StateChange %v after the disable", m)
	}
}

// A change the client makes right after enabling, on the same WebSocket, is
// pushed: the enable is in place before the next message is handled.
func TestAChangeRightAfterTheEnableIsPushed(t *testing.T) {
	beforePushBase = func() { time.Sleep(200 * time.Millisecond) }
	t.Cleanup(func() { beforePushBase = func() {} })
	f := pushServer(t, hour, 0)
	c := f.mustWS(t)
	wsSend(t, c, `{"@type":"WebSocketPushEnable","dataTypes":["Email"]}`)
	wsSend(t, c, `{"@type":"Request","id":"s","using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],`+
		`"methodCalls":[["Email/set",{"accountId":"`+testAccount+`","update":{"`+f.email+`":{"keywords/$seen":true}}},"c0"]]}`)
	if _, ok := wsNext(t, c, "StateChange", eventWait); !ok {
		t.Error("no StateChange for the change made after the enable")
	}
}

// Enabling with a pushState sends what moved since it at once; with the
// current one, nothing.
func TestWebSocketPushResumesFromAPushState(t *testing.T) {
	f := pushServer(t, hour, 0)
	c := f.mustWS(t)
	enable(t, c, `{"@type":"WebSocketPushEnable","dataTypes":["Email"]}`)
	f.flag(t, "$seen")
	m, ok := wsNext(t, c, "StateChange", eventWait)
	if !ok {
		t.Fatal("no StateChange after the flag")
	}
	old, _ := m["pushState"].(string)
	wsSend(t, c, `{"@type":"WebSocketPushDisable"}`)
	f.flag(t, "$flagged")

	for _, tc := range []struct {
		name  string
		state func() string
		want  bool
	}{
		{"a pushState from before a move", func() string { return old }, true},
		{"a pushState that is current", func() string { return "" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c2 := f.mustWS(t)
			state := tc.state()
			if state == "" {
				enable(t, c2, `{"@type":"WebSocketPushEnable","dataTypes":["Email"]}`)
				f.flag(t, "$answered")
				cur, ok := wsNext(t, c2, "StateChange", eventWait)
				if !ok {
					t.Fatal("no StateChange to take the current pushState from")
				}
				state, _ = cur["pushState"].(string)
				wsSend(t, c2, `{"@type":"WebSocketPushDisable"}`)
			}
			wsSend(t, c2, `{"@type":"WebSocketPushEnable","dataTypes":["Email"],"pushState":"`+state+`"}`)
			_, got := wsNext(t, c2, "StateChange", 300*time.Millisecond)
			if got != tc.want {
				t.Errorf("StateChange at once: %v, want %v", got, tc.want)
			}
		})
	}
}

// With push off, an enable is not a message the server knows.
func TestWebSocketPushNeedsPushOn(t *testing.T) {
	f := pushServer(t, 0, 0)
	c := f.mustWS(t)
	wsSend(t, c, `{"@type":"WebSocketPushEnable","dataTypes":null}`)
	m, ok := wsNext(t, c, "RequestError", time.Second)
	if !ok || m["type"] != jmapcore.ProblemNotRequest {
		t.Errorf("answer %v, want a notRequest RequestError", m)
	}
}

// A peer that answers no ping is closed after the timeout; one that does
// stays open.
func TestASilentWebSocketIsClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reading bool
		open    bool
	}{
		{"answering pings", true, true},
		{"silent", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := pushServer(t, hour, 0, func(o *Options) { o.WebSocketTimeout = 200 * time.Millisecond })
			c := f.mustWS(t)
			if tc.reading {
				// A reader answers pings; CloseRead keeps one running.
				_ = c.CloseRead(context.Background())
			}
			time.Sleep(time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			err := c.Write(ctx, websocket.MessageText, []byte(echoRequest))
			if tc.reading {
				if err != nil {
					t.Errorf("write: %v, want the connection open", err)
				}
				return
			}
			_, _, rerr := c.Read(ctx)
			if rerr == nil || errors.Is(rerr, context.DeadlineExceeded) {
				t.Errorf("read: %v, want the connection closed", rerr)
			}
		})
	}
}

// Shutting the server down closes its WebSockets as going away.
func TestShutdownClosesAWebSocket(t *testing.T) {
	f := pushServer(t, hour, 0)
	c := f.mustWS(t)
	f.s.stopOnce.Do(func() { close(f.s.done) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusGoingAway {
		t.Errorf("close status %v (%v), want going away", got, err)
	}
}

// Stopping waits for every WebSocket handler: a hijacked connection is
// outside what http.Server.Shutdown waits for, and a handler still running
// would touch a store the caller is about to remove.
func TestStoppingWaitsForTheWebSocketHandlers(t *testing.T) {
	before := testutil.ToFloat64(metricWebSockets)
	f := pushServer(t, hour, 0)
	for range 3 {
		c := f.mustWS(t)
		wsSend(t, c, echoRequest)
		if _, ok := wsNext(t, c, "Response", time.Second); !ok {
			t.Fatal("no Response")
		}
		// A reading client answers the close at once.
		_ = c.CloseRead(context.Background())
	}
	f.s.stopWebSockets(context.Background())
	if got := testutil.ToFloat64(metricWebSockets); got != before {
		t.Errorf("%v WebSocket handlers still running after the stop", got-before)
	}
}

// stallListener hands out connections whose writes block while stalled, as a
// client that stops reading does once the socket buffers are full.
type stallListener struct {
	net.Listener
	stalled atomic.Bool
	blocked chan *stallConn // receives each write that blocks
	release chan struct{}
}

func (l *stallListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &stallConn{Conn: c, l: l, closed: make(chan struct{})}, nil
}

type stallConn struct {
	net.Conn
	l         *stallListener
	closeOnce sync.Once
	closed    chan struct{}
}

func (c *stallConn) Write(p []byte) (int, error) {
	if c.l.stalled.Load() {
		c.l.blocked <- c
		select {
		case <-c.l.release:
		case <-c.closed:
			return 0, net.ErrClosed
		}
	}
	return c.Conn.Write(p)
}

func (c *stallConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// Disabling push while a StateChange is stuck on a client that is not reading
// stops the subscription and keeps the connection: the next request is answered.
func TestAPushDisableDuringABlockedWriteKeepsTheConnection(t *testing.T) {
	stall := &stallListener{blocked: make(chan *stallConn, 1), release: make(chan struct{})}
	wrapListener = func(l net.Listener) net.Listener { stall.Listener = l; return stall }
	stopped := make(chan struct{}, 1)
	pushStopped = func() { stopped <- struct{}{} }
	t.Cleanup(func() {
		wrapListener = func(l net.Listener) net.Listener { return l }
		pushStopped = func() {}
	})
	f := pushServer(t, hour, 0)
	c := f.mustWS(t)
	enable(t, c, `{"@type":"WebSocketPushEnable","dataTypes":["Email"]}`)

	stall.stalled.Store(true)
	f.flag(t, "$seen")
	var conn *stallConn
	select {
	case conn = <-stall.blocked:
	case <-time.After(eventWait):
		t.Fatal("the StateChange write never started")
	}
	stall.stalled.Store(false)
	wsSend(t, c, `{"@type":"WebSocketPushDisable"}`)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("the disable was not handled")
	}
	select {
	case <-conn.closed:
		t.Fatal("the disable closed the connection under a write in progress")
	case <-time.After(300 * time.Millisecond):
	}
	close(stall.release)

	wsSend(t, c, echoRequest)
	if _, ok := wsNext(t, c, "Response", time.Second); !ok {
		t.Fatal("no Response after a disable during a blocked write: the connection was closed")
	}
}

// A write stuck on a client that stopped reading ends within the timeout: the
// ping cannot get the writer, fails, and closes the connection.
func TestAStuckWriteIsClosedWithinTheTimeout(t *testing.T) {
	stall := &stallListener{blocked: make(chan *stallConn, 1), release: make(chan struct{})}
	wrapListener = func(l net.Listener) net.Listener { stall.Listener = l; return stall }
	t.Cleanup(func() { wrapListener = func(l net.Listener) net.Listener { return l } })
	f := pushServer(t, hour, 0, func(o *Options) { o.WebSocketTimeout = 400 * time.Millisecond })
	c := f.mustWS(t)
	enable(t, c, `{"@type":"WebSocketPushEnable","dataTypes":["Email"]}`)

	stall.stalled.Store(true)
	f.flag(t, "$seen")
	var conn *stallConn
	select {
	case conn = <-stall.blocked:
	case <-time.After(eventWait):
		t.Fatal("the StateChange write never started")
	}
	select {
	case <-conn.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("a write stuck past the timeout left the connection open")
	}
}
