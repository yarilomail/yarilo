package director

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/internal/cluster/proto"
)

// Rebalance moves one domain at a time and never the same domain twice in a
// row: a placement that keeps being reconsidered is a mailbox that keeps being
// disconnected. The cooldown is the hysteresis -- without it two backends
// either side of the threshold trade a domain back and forth for ever (#1943).
type domainMoves struct {
	mu    sync.Mutex
	moved map[string]time.Time
}

func (m *domainMoves) recently(domain string, cooldown time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.moved[domain]
	return ok && time.Since(at) < cooldown
}

func (m *domainMoves) note(domain string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.moved == nil {
		m.moved = make(map[string]time.Time)
	}
	m.moved[domain] = time.Now()
}

// rebalanceDomains runs one pass: while the busiest backend of a tag carries
// more than the threshold above the quietest, the smallest domain that would
// not overshoot moves down. One move per pass, so the next pass judges a load
// that has already changed.
func (s *Server) rebalanceDomains() {
	pct := s.opts.DomainRebalancePercent
	if s.assignmentPolicy() != policyDomain || pct <= 0 {
		return
	}
	total, _ := s.sessionCounts()
	byTag := make(map[string][]backendLoad)
	for _, b := range s.ring.Backends() {
		if !b.Up || b.Vhosts <= 0 {
			continue
		}
		byTag[b.Tag] = append(byTag[b.Tag], backendLoad{
			host: fmt.Sprintf("%s:%d", b.IP, b.Port),
			ip:   b.IP,
			load: total[b.IP] * 100 / b.Vhosts,
		})
	}
	for tag, loads := range byTag {
		if len(loads) < 2 {
			continue
		}
		hi, lo := loads[0], loads[0]
		for _, l := range loads[1:] {
			if l.load > hi.load {
				hi = l
			}
			if l.load < lo.load {
				lo = l
			}
		}
		if hi.load == 0 || (hi.load-lo.load)*100/hi.load < pct {
			continue
		}
		s.moveOneDomain(tag, hi, lo)
	}
}

type backendLoad struct {
	host string
	ip   string
	load int
}

// moveOneDomain takes the quietest domain off the busiest backend: moving the
// loudest would swap the imbalance rather than settle it.
func (s *Server) moveOneDomain(tag string, hi, lo backendLoad) {
	cooldown := s.opts.domainRebalanceCooldown()
	var pick string
	pickSessions := -1
	for _, domain := range s.domainDir.Domains(hi.ip) {
		if s.domainMoves.recently(domain, cooldown) {
			continue
		}
		n := s.domainSessionCount(domain, hi.ip)
		if pickSessions < 0 || n < pickSessions {
			pick, pickSessions = domain, n
		}
	}
	if pick == "" {
		return
	}
	seq, by := s.domainDir.Set(pick, lo.host)
	s.domainMoves.note(pick)
	s.membership.originate("DOMAIN-ASSIGN", fmt.Sprintf("%s\t%s\t%d\t%s",
		proto.TabEscape(pick), lo.host, seq, by))
	slog.Info("director: rebalancing a domain",
		"domain", pick, "tag", tag, "from", hi.host, "to", lo.host,
		"from_load", hi.load, "to_load", lo.load, "sessions", pickSessions)
	// After the ring knows, as the reference kills only after sending its
	// USER-MOVE: a kick that finishes instantly must not race the update.
	s.kickDomainSessions(pick, hi.host)
}

// domainSessionCount is how many sessions of a domain sit on one backend.
func (s *Server) domainSessionCount(domain, ip string) int {
	s.sessRecMu.RLock()
	defer s.sessRecMu.RUnlock()
	n := 0
	for id := range s.sessByBE[ip] {
		if rec, ok := s.sessById[id]; ok && DomainOf(rec.user) == domain {
			n++
		}
	}
	return n
}
