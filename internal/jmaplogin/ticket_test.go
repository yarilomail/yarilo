package jmaplogin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/jmapcore"
)

func TestATicketNamesItsLoginOnlyWhereItWasIssued(t *testing.T) {
	now := time.Date(2026, 10, 24, 23, 59, 30, 0, time.UTC)
	tk := newTickets("key-one", time.Minute)
	tk.now = func() time.Time { return now }
	good, _ := tk.issue("u1@example.com", "192.0.2.7")
	payload, sig, _ := strings.Cut(good, ".")
	other := newTickets("key-two", time.Minute)
	other.now = tk.now
	foreign, _ := other.issue("u1@example.com", "192.0.2.7")
	forged := base64.RawURLEncoding.EncodeToString([]byte("u2@example.com\n192.0.2.7\n9999999999")) + "." + sig
	cases := []struct {
		name, ticket, ip string
		later            time.Duration
		want             string
	}{
		{"as issued", good, "192.0.2.7", 0, "u1@example.com"},
		{"one second before the end", good, "192.0.2.7", 59 * time.Second, "u1@example.com"},
		{"at the end", good, "192.0.2.7", time.Minute, ""},
		{"from another address", good, "192.0.2.8", 0, ""},
		{"another key", foreign, "192.0.2.7", 0, ""},
		{"payload swapped", forged, "192.0.2.7", 0, ""},
		{"signature cut", payload + "." + sig[:len(sig)-2], "192.0.2.7", 0, ""},
		{"no signature", payload, "192.0.2.7", 0, ""},
		{"not base64", "!!!." + sig, "192.0.2.7", 0, ""},
		{"empty", "", "192.0.2.7", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tk.now = func() time.Time { return now.Add(tc.later) }
			got, err := tk.check(tc.ticket, tc.ip)
			if got != tc.want || (tc.want == "") != errors.Is(err, errBadTicket) {
				t.Errorf("check = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestTicketsNeedAKeyAndATTL(t *testing.T) {
	if newTickets("", time.Minute) != nil || newTickets("k", 0) != nil || newTickets("k", time.Minute) == nil {
		t.Error("tickets are issued without a key or a TTL, or not with both")
	}
}

// startTicketProxy fronts a backend that echoes the user and the query it got.
func startTicketProxy(t *testing.T, ttl time.Duration) string {
	t.Helper()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Echo-User", r.Header.Get(hdrUser))
		w.Header().Set("Echo-Query", r.URL.RawQuery)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(b.Close)
	return startProxy(t, Options{Router: StaticRouter{Addr: b.Listener.Addr().String()},
		WebSocketTicketKey: "test-key", WebSocketTicketTTL: ttl})
}

type sent struct {
	status int
	header http.Header
	body   []byte
}

func send(t *testing.T, method, url, auth string) sent {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	body, _ := io.ReadAll(resp.Body)
	return sent{resp.StatusCode, resp.Header, body}
}

func issueTicket(t *testing.T, base string) string {
	t.Helper()
	resp := send(t, http.MethodPost, base+jmapcore.WebSocketTicketPath, basic("u1", "pw"))
	if resp.status != http.StatusOK || resp.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("ticket: status %d, Cache-Control %q", resp.status, resp.header.Get("Cache-Control"))
	}
	var got struct{ Value, ValidUntil, Username, ClientAddress string }
	if err := json.Unmarshal(resp.body, &got); err != nil {
		t.Fatal(err)
	}
	until, err := time.Parse(time.RFC3339, got.ValidUntil)
	if got.Value == "" || got.Username != "u1" || got.ClientAddress != "127.0.0.1" || err != nil ||
		time.Until(until) > time.Minute || time.Until(until) < 58*time.Second {
		t.Fatalf("ticket %+v", got)
	}
	return got.Value
}

// A ticket opens the WebSocket path as its login, without credentials, and the
// backend never sees it.
func TestATicketOpensTheWebSocketPath(t *testing.T) {
	base := startTicketProxy(t, time.Minute)
	ticket := issueTicket(t, base)
	resp := send(t, http.MethodGet, base+jmapcore.WebSocketPath+"?ticket="+ticket, "")
	if resp.status != http.StatusOK || resp.header.Get("Echo-User") != "u1" {
		t.Fatalf("status %d as %q, want the ticket's login", resp.status, resp.header.Get("Echo-User"))
	}
	if q := resp.header.Get("Echo-Query"); strings.Contains(q, "ticket") {
		t.Errorf("the backend got the ticket: %q", q)
	}
}

func TestTicketRefusals(t *testing.T) {
	base := startTicketProxy(t, time.Minute)
	ticket := issueTicket(t, base)
	cases := []struct {
		name, method, path, auth string
		want                     int
	}{
		{"issued only to a login", http.MethodPost, jmapcore.WebSocketTicketPath, "", http.StatusUnauthorized},
		{"issued only to a right password", http.MethodPost, jmapcore.WebSocketTicketPath, basic("u1", "no"), http.StatusUnauthorized},
		{"issued only on POST", http.MethodGet, jmapcore.WebSocketTicketPath, basic("u1", "pw"), http.StatusMethodNotAllowed},
		{"valid only for the WebSocket", http.MethodPost, "/jmap/api/?ticket=" + ticket, "", http.StatusUnauthorized},
		{"valid only for the WebSocket, session", http.MethodGet, jmapcore.SessionPath + "?ticket=" + ticket, "", http.StatusUnauthorized},
		{"forged", http.MethodGet, jmapcore.WebSocketPath + "?ticket=x" + ticket, "", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := send(t, tc.method, base+tc.path, tc.auth).status; got != tc.want {
				t.Errorf("status %d, want %d", got, tc.want)
			}
		})
	}
}

// With the TTL at 0 nothing is issued and a ticket opens nothing.
func TestTicketsOff(t *testing.T) {
	on := startTicketProxy(t, time.Minute)
	ticket := issueTicket(t, on)
	off := startTicketProxy(t, 0)
	if got := send(t, http.MethodPost, off+jmapcore.WebSocketTicketPath, basic("u1", "pw")).status; got != http.StatusNotFound {
		t.Errorf("issue with tickets off: %d, want 404", got)
	}
	if got := send(t, http.MethodGet, off+jmapcore.WebSocketPath+"?ticket="+ticket, "").status; got != http.StatusUnauthorized {
		t.Errorf("ticket with tickets off: %d, want 401", got)
	}
}
