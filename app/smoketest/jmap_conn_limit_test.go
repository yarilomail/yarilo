package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// limitServer stands in for jmap-login: it counts open connections and refuses
// a request over limit with status and retryAfter. leak keeps a closed
// connection counted, as a server that never releases a slot would.
type limitServer struct {
	limit      int
	status     int
	retryAfter string
	leak       bool

	mu   sync.Mutex
	open map[net.Conn]bool
}

func (l *limitServer) state(c net.Conn, s http.ConnState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch s {
	case http.StateNew:
		l.open[c] = true
	case http.StateClosed, http.StateHijacked:
		if !l.leak {
			delete(l.open, c)
		}
	}
}

func (l *limitServer) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	l.mu.Lock()
	n := len(l.open)
	l.mu.Unlock()
	if l.limit > 0 && n > l.limit {
		if l.retryAfter != "" {
			w.Header().Set("Retry-After", l.retryAfter)
		}
		w.WriteHeader(l.status)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func TestJMAPConnLimitRow(t *testing.T) {
	tests := []struct {
		name    string
		srv     *limitServer
		rowSays int
		wantErr string
	}{
		{"429 with Retry-After at the limit", &limitServer{limit: 3, status: 429, retryAfter: "10"}, 3, ""},
		{"a slot already taken refuses early", &limitServer{limit: 2, status: 429, retryAfter: "10"}, 3, ""},
		{"503 for the limit", &limitServer{limit: 3, status: 503, retryAfter: "10"}, 3, "want 429"},
		{"429 without Retry-After", &limitServer{limit: 3, status: 429}, 3, "Retry-After"},
		{"no limit enforced", &limitServer{status: 429, retryAfter: "10"}, 3, "passed a limit of 3"},
		{"a closed connection keeps its slot", &limitServer{limit: 3, status: 429, retryAfter: "10", leak: true}, 3, "still answers HTTP 429"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := tc.srv
			srv.open = map[net.Conn]bool{}
			ts := httptest.NewUnstartedServer(srv)
			ts.Config.ConnState = srv.state
			ts.StartTLS()
			t.Cleanup(ts.Close)
			tlsConf := ts.Client().Transport.(*http.Transport).TLSClientConfig
			err := jmapConnLimitAt(ts.URL+"/.well-known/jmap", tlsConf, "u1", "pw", tc.rowSays, time.Second)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("row failed: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("row = %v, want an error naming %q", err, tc.wantErr)
			}
		})
	}
}
