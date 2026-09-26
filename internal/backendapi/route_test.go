package backendapi

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fixedRouter answers every lookup with one pod, or with an error.
type fixedRouter struct {
	owner string
	err   error
}

func (f fixedRouter) Lookup(string) (string, error) { return f.owner, f.err }

// peer stands for another pod's backend-api and keeps what reached it.
type peer struct {
	mu    sync.Mutex
	paths []string
	body  []string
	hdr   []string
}

func (p *peer) serve(t *testing.T) (host, port string) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.paths = append(p.paths, r.URL.RequestURI())
		p.body = append(p.body, string(b))
		p.hdr = append(p.hdr, r.Header.Get(routedHeader))
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"served":"peer"}`))
	}))
	t.Cleanup(ts.Close)
	host, port, err := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

// A request about a user runs where the director keeps them: here, forwarded,
// or refused when the director cannot be asked (#2053).
func TestARequestAboutAUserRunsWhereTheUserLives(t *testing.T) {
	const self = "10.9.9.1"
	for _, tc := range []struct {
		name      string
		router    func(peerHost string) UserRouter
		routed    bool
		wantLocal int
		wantPeer  int
		wantCode  int
	}{
		{"no director: runs here", func(string) UserRouter { return nil }, false, 1, 0, http.StatusOK},
		{"the user lives here", func(string) UserRouter { return fixedRouter{owner: self} }, false, 1, 0, http.StatusOK},
		{"the user lives on the other pod", func(h string) UserRouter { return fixedRouter{owner: h} }, false, 0, 1, http.StatusOK},
		{"the director cannot be asked", func(string) UserRouter { return fixedRouter{err: errors.New("down")} }, false, 0, 0, http.StatusServiceUnavailable},
		{"already forwarded once: runs here", func(h string) UserRouter { return fixedRouter{owner: h} }, true, 1, 0, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, fake, _, srv := ftsTestServerOf(t)
			p := &peer{}
			host, port := p.serve(t)
			srv.opts.Router, srv.opts.PodIP, srv.opts.PeerPort = tc.router(host), self, port

			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/backend/fts/optimize?user=alice@example.com", nil)
			if tc.routed {
				req.Header.Set(routedHeader, "10.9.9.2")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close() //nolint:errcheck
			if resp.StatusCode != tc.wantCode {
				t.Errorf("answered %d, want %d", resp.StatusCode, tc.wantCode)
			}
			fake.mu.Lock()
			local := fake.optimize
			fake.mu.Unlock()
			if local != tc.wantLocal || len(p.paths) != tc.wantPeer {
				t.Errorf("ran here %d times and on the other pod %d, want %d and %d", local, len(p.paths), tc.wantLocal, tc.wantPeer)
			}
			if tc.wantPeer == 1 && len(p.paths) == 1 && (p.paths[0] != "/api/backend/fts/optimize?user=alice@example.com" || p.hdr[0] != self) {
				t.Errorf("the other pod got %q marked %q, want the same request marked from %s", p.paths[0], p.hdr[0], self)
			}
		})
	}
}

// A user named in a JSON body is routed too, and the body reaches the other
// pod whole.
func TestAUserInTheBodyIsRoutedWithItsBody(t *testing.T) {
	ts, _, _, srv := ftsTestServerOf(t)
	p := &peer{}
	host, port := p.serve(t)
	srv.opts.Router, srv.opts.PodIP, srv.opts.PeerPort = fixedRouter{owner: host}, "10.9.9.1", port

	body := `{"user":"alice@example.com","folder":"Projects"}`
	resp, err := http.Post(ts.URL+"/api/backend/folder/create", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() //nolint:errcheck
	if len(p.body) != 1 || p.body[0] != body {
		t.Errorf("the other pod got %q, want the body as sent", p.body)
	}
}
