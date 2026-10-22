package jmaplogin

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// streamBackend sends a state event every interval until the client goes.
func streamBackend(t *testing.T, interval time.Duration) string {
	t.Helper()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		for i := 1; ; i++ {
			if _, err := fmt.Fprintf(w, "id: %d\nevent: state\ndata: {}\n\n", i); err != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(interval):
			}
		}
	}))
	t.Cleanup(b.Close)
	return b.Listener.Addr().String()
}

// startServer runs the proxy on a real socket and returns it with its URL.
func startServer(t *testing.T, opts Options) (*Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	opts.Addr = ln.Addr().String()
	_ = ln.Close()
	if opts.Auth == nil {
		opts.Auth = fakeAuth{users: map[string]string{"u1": "pw"}}
	}
	s := New(opts)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	for i := 0; i < 300; i++ {
		if c, err := net.Dial("tcp", opts.Addr); err == nil {
			_ = c.Close()
			return s, "http://" + opts.Addr
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("proxy never accepted")
	return nil, ""
}

// openStream returns a channel of event ids, closed when the stream ends.
func openStream(t *testing.T, base string) <-chan string {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, base+eventSourcePath+"?types=*", nil)
	req.Header.Set("Authorization", basic("u1", "pw"))
	resp, err := (&http.Client{Transport: &http.Transport{}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() }) //nolint:errcheck
	ids := make(chan string, 64)
	go func() {
		defer close(ids)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if id, ok := strings.CutPrefix(sc.Text(), "id: "); ok {
				ids <- id
			}
		}
	}()
	return ids
}

func nextID(ids <-chan string, within time.Duration) (string, bool) {
	select {
	case id, ok := <-ids:
		return id, ok
	case <-time.After(within):
		return "", false
	}
}

// ended reports whether the stream closes within the wait, skipping events.
func ended(ids <-chan string, within time.Duration) bool {
	deadline := time.After(within)
	for {
		select {
		case _, ok := <-ids:
			if !ok {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func TestStreamPassesThroughEventByEvent(t *testing.T) {
	_, base := startServer(t, Options{Router: StaticRouter{Addr: streamBackend(t, 100*time.Millisecond)}})
	ids := openStream(t, base)
	for _, want := range []string{"1", "2", "3"} {
		if got, ok := nextID(ids, time.Second); !ok || got != want {
			t.Fatalf("event %q, want %q: the proxy held the stream back", got, want)
		}
	}
}

func TestStreamOutlivesTheProxyTimeouts(t *testing.T) {
	old := serverTimeout
	serverTimeout = 300 * time.Millisecond
	t.Cleanup(func() { serverTimeout = old })
	_, base := startServer(t, Options{Router: StaticRouter{Addr: streamBackend(t, 100*time.Millisecond)}})
	ids := openStream(t, base)
	time.Sleep(time.Second)
	// What arrived before the timeout is buffered; only a later event counts.
	for drained := false; !drained; {
		select {
		case _, ok := <-ids:
			if !ok {
				t.Fatal("the stream ended at the proxy's timeout")
			}
		default:
			drained = true
		}
	}
	if _, ok := nextID(ids, time.Second); !ok {
		t.Fatal("no event after the proxy's timeout")
	}
}

// A kick by session id, as warden delivers one, ends the stream.
func TestSessionKickEndsTheStream(t *testing.T) {
	w := &fakeWarden{}
	s, base := startServer(t, Options{Router: StaticRouter{Addr: streamBackend(t, 100*time.Millisecond)}, Warden: w})
	ids := openStream(t, base)
	if _, ok := nextID(ids, time.Second); !ok {
		t.Fatal("no event before the kick")
	}
	calls := w.snapshot()
	if len(calls) == 0 {
		t.Fatal("the stream's connection was never accounted")
	}
	if !s.kickSession(calls[0].id) {
		t.Fatal("no connection under the accounted session id")
	}
	if !ended(ids, time.Second) {
		t.Error("the stream outlived the kick")
	}
}

// The director kicks a user with a reason; a move must end the stream so the
// client reconnects to the user's new backend.
func TestDirectorKickEndsTheStream(t *testing.T) {
	for _, push := range []string{
		"USER-KICKED\tu1\treason=move",
		"USER-KICKED\tu1\treason=admin",
		"USER-KICKED\tu1",
	} {
		t.Run(push, func(t *testing.T) {
			s, base := startServer(t, Options{Router: StaticRouter{Addr: streamBackend(t, 100*time.Millisecond)}, Warden: &fakeWarden{}})
			ids := openStream(t, base)
			if _, ok := nextID(ids, time.Second); !ok {
				t.Fatal("no event before the kick")
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			s.WatchKicks(ctx, Kicks{DirectorAddr: fakeDirector(t, push)})
			if !ended(ids, 2*time.Second) {
				t.Error("the stream outlived the director's kick")
			}
		})
	}
}

// Another user's kick leaves the stream alone.
func TestDirectorKickOfAnotherUserKeepsTheStream(t *testing.T) {
	s, base := startServer(t, Options{Router: StaticRouter{Addr: streamBackend(t, 100*time.Millisecond)}, Warden: &fakeWarden{}})
	ids := openStream(t, base)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s.WatchKicks(ctx, Kicks{DirectorAddr: fakeDirector(t, "USER-KICKED\tu2\treason=move")})
	if ended(ids, 500*time.Millisecond) {
		t.Error("u2's kick ended u1's stream")
	}
}

// fakeDirector completes the handshake, pushes lines and holds the connection.
func fakeDirector(t *testing.T, lines ...string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close() //nolint:errcheck
		_, _ = conn.Write([]byte("DONE\n"))
		rd := bufio.NewReader(conn)
		for {
			line, err := rd.ReadString('\n')
			if err != nil || line == "DONE\n" {
				break
			}
		}
		for _, l := range lines {
			_, _ = conn.Write([]byte(l + "\n"))
		}
		_, _ = rd.ReadString('\n')
	}()
	return ln.Addr().String()
}
