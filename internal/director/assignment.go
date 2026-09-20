package director

import (
	"fmt"
	"net"
	"sort"
	"strconv"

	"github.com/yarilomail/yarilo/internal/cluster/proto"
	"github.com/yarilomail/yarilo/internal/cluster/ring"
)

// Assignment policies for the INITIAL (unpinned) user→backend placement (#797).
// Sticky pins and USER-MOVE are unaffected — only a fresh
// assignment consults the policy.
const (
	policyHash          = "hash"           // consistent hash (default; reference semantics)
	policyLeastSessions = "least_sessions" // load-aware, two-level normalized
	policyDomain        = "domain"         // one backend per domain, placed by load (#1943)
)

func (s *Server) assignmentPolicy() string {
	switch s.opts.AssignmentPolicy {
	case policyLeastSessions:
		return policyLeastSessions
	case policyDomain:
		return policyDomain
	}
	return policyHash
}

// domainLoad is what a placement compares: the sessions a backend carries plus
// the placements it has been given that no session has reported yet. The
// second half is what keeps a burst of first logins off one backend -- the
// reference credits the host inside user_directory_add, not on connect.
func (s *Server) domainLoad(b ring.Backend, total map[string]int) int {
	return total[b.IP] + s.domainDir.Pending(b.IP)
}

// placeDomain answers where a domain belongs: its standing backend while that
// backend is still serving, else the least loaded one in the tag. A placement
// is recorded and propagated by the caller.
func (s *Server) placeDomain(domain, tag string) *ring.Backend {
	if e := s.domainDir.Get(domain); e != nil {
		if b := s.backendAt(e.Host, tag); b != nil {
			return b
		}
		// The backend left the ring: the domain is placed afresh here, on this
		// login, rather than in a wave when it went down (#1943).
		s.domainDir.Delete(domain)
	}
	return s.leastLoadedInTag(tag)
}

// backendAt finds an Up backend of the tag by "ip:port".
func (s *Server) backendAt(host, tag string) *ring.Backend {
	ip, portStr, err := net.SplitHostPort(host)
	if err != nil {
		return nil
	}
	port, perr := strconv.Atoi(portStr)
	if perr != nil {
		return nil
	}
	for _, b := range s.ring.Backends() {
		if b.Up && b.Tag == tag && b.Vhosts > 0 && b.IP == ip && b.Port == port {
			found := b
			return &found
		}
	}
	return nil
}

// leastLoadedInTag is the fewest-connections choice, ties by (IP, port) so two
// directors placing the same domain at once agree.
func (s *Server) leastLoadedInTag(tag string) *ring.Backend {
	total, _ := s.sessionCounts()
	var best *ring.Backend
	bestLoad := 0
	for _, b := range s.ring.Backends() {
		if !b.Up || b.Tag != tag || b.Vhosts <= 0 {
			continue
		}
		load := s.domainLoad(b, total) * 100 / b.Vhosts
		cand := b
		switch {
		case best == nil, load < bestLoad,
			load == bestLoad && (cand.IP < best.IP || (cand.IP == best.IP && cand.Port < best.Port)):
			best, bestLoad = &cand, load
		}
	}
	return best
}

// pickBackend chooses the backend for an UNPINNED user within the requested tag.
//
//   - hash (default): the consistent-hash backend for the tag.
//   - least_sessions: the least-loaded Up backend in the tag by a two-level,
//     capacity-normalized load. Level 1 = the requested protocol's sessions;
//     level 2 = total sessions, deciding among level-1 ties. Each load is
//     normalized as count*100/vhosts (vhosts 1..100; 0 = drain → excluded).
//     Tie-break: lower (IP, port). When reqProto is "" (admin path — no protocol)
//     level 1 is skipped and total load decides.
//
// Strict tag match: candidates are only Up backends whose Tag == tag ("" is a
// real tag, the untagged pool) with vhosts > 0. No full-ring fallback — an empty
// candidate set returns nil and the caller FAILs.
func (s *Server) pickBackend(user, tag, reqProto string) *ring.Backend {
	switch s.assignmentPolicy() {
	case policyDomain:
		if domain := DomainOf(user); domain != "" {
			return s.placeDomain(domain, tag)
		}
		// No domain to be affine to: the hash answers, as it always did.
		return s.ring.LookupBackendByTag(user, tag)
	case policyLeastSessions:
	default:
		return s.ring.LookupBackendByTag(user, tag)
	}

	total, byProto := s.sessionCounts()
	type cand struct {
		b      ring.Backend
		l1, l2 int
	}
	var cands []cand
	for _, b := range s.ring.Backends() {
		if !b.Up || b.Tag != tag || b.Vhosts <= 0 {
			continue // strict tag; drain (vhosts 0) and down backends excluded
		}
		pc := 0
		if m := byProto[b.IP]; m != nil {
			pc = m[reqProto]
		}
		cands = append(cands, cand{
			b:  b,
			l1: pc * 100 / b.Vhosts,
			l2: total[b.IP] * 100 / b.Vhosts,
		})
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool {
		ci, cj := cands[i], cands[j]
		if reqProto != "" && ci.l1 != cj.l1 {
			return ci.l1 < cj.l1
		}
		if ci.l2 != cj.l2 {
			return ci.l2 < cj.l2
		}
		if ci.b.IP != cj.b.IP {
			return ci.b.IP < cj.b.IP
		}
		return ci.b.Port < cj.b.Port
	})
	best := cands[0].b
	return &best
}

// recordDomain writes and propagates a domain's placement, and only when it
// moved or is new: every login of a placed domain would otherwise originate a
// ring update saying what the ring already knows.
func (s *Server) recordDomain(user, addr string) {
	if s.assignmentPolicy() != policyDomain {
		return
	}
	domain := DomainOf(user)
	if domain == "" {
		return
	}
	if e := s.domainDir.Get(domain); e != nil && e.Host == addr {
		s.domainDir.Touch(domain)
		return
	}
	seq, by := s.domainDir.Set(domain, addr)
	s.membership.originate("DOMAIN-ASSIGN", fmt.Sprintf("%s\t%s\t%d\t%s",
		proto.TabEscape(domain), addr, seq, by))
}

// assignAndPin resolves an UNPINNED user via the policy, records the pin, and
// propagates USER-ASSIGN — the SINGLE owner of initial placement (#797). Every
// fresh-assignment caller (login LOOKUP, LMTP RouteUser, admin apiMap under
// least_sessions) funnels through here so none can independently pick a
// different pod and split a user's per-user writer (#788). Returns nil when no
// backend is available.
func (s *Server) assignAndPin(user, tag, reqProto string) *ring.Backend {
	b := s.pickBackend(user, tag, reqProto)
	if b == nil {
		return nil
	}
	addr := net.JoinHostPort(b.IP, strconv.Itoa(b.Port))
	s.recordDomain(user, addr)
	h := s.userDir.Set(user, addr, false)
	if seq, by, ok := s.userDir.LastAssign(h); ok {
		s.membership.originate("USER-ASSIGN", fmt.Sprintf("%d\t%s\t%d\t%s", h, addr, seq, by))
	}
	return b
}
