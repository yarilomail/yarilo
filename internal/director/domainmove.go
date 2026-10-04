package director

import (
	"log/slog"
)

// kickDomainSessions ends every session of a domain that is still on the
// backend it has left. One user at a time, through the same kill the admin
// path uses: the confirmed kick (#847), the ring-wide LOOKUP hold (#858) and
// the flush (#848) are proven per user, and a domain move is rare enough not
// to be worth a second key in that machinery (#1943).
func (s *Server) kickDomainSessions(domain, oldHost string) {
	ip := hostIP(oldHost)
	s.sessRecMu.RLock()
	users := make(map[string]struct{})
	for id := range s.sessByBE[ip] {
		if rec, ok := s.sessById[id]; ok && DomainOf(rec.user) == domain {
			users[rec.user] = struct{}{}
		}
	}
	s.sessRecMu.RUnlock()
	if len(users) == 0 {
		return
	}
	slog.Info("director: domain moved, kicking its sessions off the old backend",
		"domain", domain, "old_backend", ip, "users", len(users))
	for user := range users {
		hash := HashUsername(user, s.hf)
		// The hold first, then the kick: a session opening in between would
		// otherwise reach the backend the domain has just left.
		s.startKilling(hash)
		s.kickStaleSessions(hash, oldHost)
	}
}
