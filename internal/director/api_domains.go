package director

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
)

// apiDomainMove places a domain on another backend of its tag and ends its
// sessions on the one it leaves, through the rebalancer's own path (#1943).
func (s *Server) apiDomainMove(w http.ResponseWriter, r *http.Request) {
	if s.assignmentPolicy() != policyDomain {
		apiError(w, "assignment_policy is not domain: no domain is placed", http.StatusConflict)
		return
	}
	domain := strings.ToLower(r.PathValue("domain"))
	var req struct {
		Backend string `json:"backend"` // backend IP; a port, if given, is ignored
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Backend == "" {
		apiError(w, "backend required", http.StatusBadRequest)
		return
	}
	ip := req.Backend
	if h, _, err := net.SplitHostPort(req.Backend); err == nil {
		ip = h
	}
	to := s.ring.GetBackend(ip)
	if to == nil || !to.Up {
		apiError(w, "backend "+ip+" is not up in the ring", http.StatusNotFound)
		return
	}
	cur := s.domainDir.Get(domain)
	if cur == nil {
		apiError(w, "domain "+domain+" is not placed", http.StatusNotFound)
		return
	}
	from := s.ring.GetBackend(hostIP(cur.Host))
	if from != nil && from.Tag != to.Tag {
		apiError(w, fmt.Sprintf("backend %s is in tag %q, the domain in %q", ip, to.Tag, from.Tag), http.StatusBadRequest)
		return
	}
	toHost := net.JoinHostPort(to.IP, strconv.Itoa(to.Port))
	if cur.Host == toHost {
		apiJSON(w, map[string]string{"status": "ok", "from": cur.Host, "to": toHost, "moved": "false"})
		return
	}
	slog.Info("director API: moving a domain", "domain", domain, "from", cur.Host, "to", toHost)
	s.moveDomain(domain, cur.Host, toHost)
	apiJSON(w, map[string]string{"status": "ok", "from": cur.Host, "to": toHost, "moved": "true"})
}
