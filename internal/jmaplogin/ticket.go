package jmaplogin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/jmapcore"
)

var errBadTicket = fmt.Errorf("%w: websocket ticket refused", errUnauthorized)

// tickets issues and checks WebSocket tickets: the login, the client address
// and an expiry, signed with the installation's key so any replica can check.
type tickets struct {
	key []byte
	ttl time.Duration
	now func() time.Time
}

// newTickets returns nil, issuing none, without a TTL or a key.
func newTickets(key string, ttl time.Duration) *tickets {
	if ttl <= 0 || key == "" {
		return nil
	}
	return &tickets{key: []byte(key), ttl: ttl, now: time.Now}
}

func (t *tickets) sign(payload string) []byte {
	m := hmac.New(sha256.New, t.key)
	m.Write([]byte(payload))
	return m.Sum(nil)
}

func (t *tickets) issue(user, ip string) (string, time.Time) {
	until := t.now().Add(t.ttl).Truncate(time.Second)
	payload := user + "\n" + ip + "\n" + strconv.FormatInt(until.Unix(), 10)
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(payload)) + "." + enc.EncodeToString(t.sign(payload)), until
}

// check returns the login a ticket names when it is signed with this key, not
// yet expired, and presented from the address it was issued to.
func (t *tickets) check(ticket, ip string) (string, error) {
	enc := base64.RawURLEncoding
	p64, s64, ok := strings.Cut(ticket, ".")
	if !ok {
		return "", errBadTicket
	}
	payload, err1 := enc.DecodeString(p64)
	sig, err2 := enc.DecodeString(s64)
	if err1 != nil || err2 != nil || !hmac.Equal(sig, t.sign(string(payload))) {
		return "", errBadTicket
	}
	parts := strings.Split(string(payload), "\n")
	if len(parts) != 3 {
		return "", errBadTicket
	}
	until, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || !t.now().Before(time.Unix(until, 0)) || parts[1] != ip {
		return "", errBadTicket
	}
	return parts[0], nil
}

// wsTicket is the ticket a WebSocket request presents instead of credentials.
func (s *Server) wsTicket(r *http.Request) string {
	if s.tickets == nil || r.URL.Path != jmapcore.WebSocketPath || r.Header.Get("Authorization") != "" {
		return ""
	}
	return r.URL.Query().Get("ticket")
}

// dropTicket keeps the ticket out of the proxied request and its logs.
func dropTicket(r *http.Request) {
	q := r.URL.Query()
	q.Del("ticket")
	r.URL.RawQuery = q.Encode()
}

// handleTicket issues a ticket to a request that authenticates as usual.
func (s *Server) handleTicket(w http.ResponseWriter, r *http.Request, st *connState, clientIP string) {
	if s.tickets == nil {
		jmapcore.WriteProblem(w, http.StatusNotFound, "Not found")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		jmapcore.WriteProblem(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	username, err := s.authenticate(r, clientIP, sessionOf(st))
	if err != nil {
		if errors.Is(err, errTemporary) {
			jmapcore.WriteProblem(w, http.StatusServiceUnavailable, "Authentication temporarily unavailable")
			return
		}
		s.challenge(w, errors.Is(err, errBadToken))
		jmapcore.WriteProblem(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	value, until := s.tickets.issue(username, clientIP)
	slog.Debug("jmap-login: websocket ticket issued", "user", username, "ip", clientIP, "session", sessionOf(st))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"value":         value,
		"clientAddress": clientIP,
		"generatedOn":   s.tickets.now().UTC().Format(time.RFC3339),
		"validUntil":    until.UTC().Format(time.RFC3339),
		"username":      username,
	})
}
