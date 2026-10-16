package director

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	gone  = "10.0.0.1"
	alive = "10.0.0.2"
)

// pinnedOnGone places one user, and so its domain, on the backend about to
// leave, with a session this director's login client owns.
func pinnedOnGone(t *testing.T) (*Server, *captureConn) {
	t.Helper()
	s := domainServer(t, gone, alive)
	if got := lookup(s, "u1@one.test"); !strings.Contains(got, "HOST\t7\t"+gone+"\t") {
		t.Fatalf("first login went to %q, want %s", got, gone)
	}
	login := &captureConn{}
	s.sessRecMu.Lock()
	s.sessById["s1"] = &sessionRec{id: "s1", user: "u1@one.test", backend: gone, proto: "imap", cl: &client{conn: login}}
	s.sessByBE[gone] = map[string]bool{"s1": true}
	s.sessRecMu.Unlock()
	return s, login
}

// A backend that left takes its users and domains with it on every director,
// as the reference's host removal frees them; a refresh of the sessions it
// had must not bring the pin back, and the next login goes elsewhere (#2255).
func TestARemovedBackendTakesItsPlacementsWithIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove func(s *Server)
	}{
		{"BACKEND-DOWN on this director", func(s *Server) {
			s.handleBackendDown(&client{conn: &captureConn{}}, []string{"BACKEND-DOWN", gone})
		}},
		{"RING-CHANGE down from a peer", func(s *Server) {
			applyRingChangeFields(s, []string{gone, "down", "a"})
		}},
		{"eviction by corroborated unreachable reports", func(s *Server) {
			s.evictUnreachable(gone, nil)
		}},
		{"lease expiry", func(s *Server) {
			s.recordBackendSeen(gone, 1)
			s.backendSeenMu.Lock()
			s.backendSeen[gone] = backendLease{seq: 1, at: time.Now().Add(-time.Hour)}
			s.backendSeenMu.Unlock()
			s.expireStaleBackends(time.Minute)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, login := pinnedOnGone(t)
			tc.remove(s)
			s.refreshPinnedSessions()
			if e := s.userDir.Get("u1@one.test"); e != nil {
				t.Errorf("the user is still pinned to %s after its backend left", e.Host)
			}
			if e := s.domainDir.Get("one.test"); e != nil {
				t.Errorf("the domain still sits on %s after its backend left", e.Host)
			}
			if !strings.Contains(string(login.written), "USER-KICKED\tu1@one.test") {
				t.Errorf("this director's session on the gone backend was not kicked: %q", login.written)
			}
			if got := lookup(s, "u1@one.test"); !strings.Contains(got, "HOST\t7\t"+alive+"\t") {
				t.Errorf("the next login after the removal got %q, want %s", got, alive)
			}
		})
	}
}

// handshakeHost is the HOST line a director sends for a backend with a lease at
// seq; a peer that predates the S field drops it and parses the rest.
func handshakeHost(ip string, seq uint64) string {
	return fmt.Sprintf("HOST\t%s\t10143\ta\tD0\tU1\t\tV100\tS%d", ip, seq)
}

// One registration of the backend that leaves: seeded from its start time, its
// last heartbeat a hundred later, as the sidecar counts them.
var (
	start = uint64(time.Now().Add(-time.Hour).Unix())
	last  = start + 100
)

// removed puts a backend on the ring with a lease at lease (none when 0) and
// removes it by BACKEND-DOWN carrying down (none when 0).
func removed(t *testing.T, lease, down uint64) *Server {
	t.Helper()
	s := domainServer(t, gone, alive)
	if lease > 0 {
		s.recordBackendSeen(gone, lease)
	}
	fields := []string{"BACKEND-DOWN", gone}
	if down > 0 {
		fields = append(fields, fmt.Sprint(down))
	}
	s.handleBackendDown(&client{conn: &captureConn{}}, fields)
	return s
}

// gossipRemoved is a director that learns the removal only from a peer's
// RING-CHANGE down, never having seen the backend's heartbeats.
func gossipRemoved(t *testing.T) *Server {
	t.Helper()
	s := domainServer(t, gone, alive)
	applyRingChangeFields(s, []string{gone, "down", "a", fmt.Sprint(last)})
	return s
}

// Every way a removed backend can reach the ring again with a record of the
// registration that left: none may re-add it, or its address is handed out for
// a pod that is not there (#2255). The stand's ghost came by the handshake.
func TestARemovedBackendDoesNotComeBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    func(t *testing.T) *Server
		back func(s *Server)
	}{
		{"handshake from a director that still holds it", func(t *testing.T) *Server { return removed(t, 0, last) }, func(s *Server) {
			applyHandshakeHost(s, handshakeHost(gone, last))
		}},
		{"handshake from a pre-#2255 director, no seq", func(t *testing.T) *Server { return removed(t, 0, last) }, func(s *Server) {
			applyHandshakeHost(s, fmt.Sprintf("HOST\t%s\t10143\ta\tD0\tU1\t\tV100", gone))
		}},
		{"anti-entropy snapshot with its last heartbeat", func(t *testing.T) *Server { return removed(t, 0, last) }, func(s *Server) {
			s.applyBackendRecord(backendRecord{ip: gone, port: 10143, tag: "a", vhosts: 100, up: true, seq: last})
		}},
		{"late RING-CHANGE up, the removal known only from gossip", gossipRemoved, func(s *Server) {
			applyRingChangeFields(s, []string{gone, "up", "a", fmt.Sprint(last), "10143", "100"})
		}},
		{"late RING-CHANGE up older than the lease at removal", func(t *testing.T) *Server { return removed(t, last, 0) }, func(s *Server) {
			applyRingChangeFields(s, []string{gone, "up", "a", fmt.Sprint(last - 10), "10143", "100"})
		}},
		{"a heartbeat sent before the removal, delivered after it", func(t *testing.T) *Server { return removed(t, 0, last) }, func(s *Server) {
			s.handleBackendUp(&client{conn: &captureConn{}}, []string{"BACKEND-UP", gone, "10143", "a", "100", fmt.Sprint(last)})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.s(t)
			tc.back(s)
			if b := s.ring.GetBackend(gone); b != nil {
				t.Fatalf("the removed backend is back in the ring: %+v", *b)
			}
		})
	}
}

// A real re-registration still comes back: a pod restarted on the same address
// seeds its seq from its own clock at start, past every seq the removal saw.
// The comparison is between two seqs of one sidecar, not between clocks.
func TestARestartedBackendComesBack(t *testing.T) {
	restart := fmt.Sprint(time.Now().Unix())
	for _, tc := range []struct {
		name string
		s    func(t *testing.T) *Server
		back func(s *Server)
	}{
		{"its registration on this director", func(t *testing.T) *Server { return removed(t, 0, last) }, func(s *Server) {
			s.handleBackendUp(&client{conn: &captureConn{}}, []string{"BACKEND-UP", gone, "10143", "a", "100", restart})
		}},
		{"its heartbeat gossiped, the removal known only from gossip", gossipRemoved, func(s *Server) {
			applyRingChangeFields(s, []string{gone, "up", "a", restart, "10143", "100"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.s(t)
			tc.back(s)
			if s.ring.GetBackend(gone) == nil {
				t.Fatal("a registration newer than the removal was refused")
			}
		})
	}
}

// The tombstone outlives backend_expire, the longest a director holding the
// backend keeps it past its last heartbeat, not a fixed five minutes (#2255).
func TestTheTombstoneOutlivesBackendExpire(t *testing.T) {
	s := removed(t, 0, last)
	s.opts.BackendExpire = 10 * time.Minute
	s.backendTombMu.Lock()
	tomb := s.backendTomb[gone]
	tomb.at = time.Now().Add(-6 * time.Minute)
	s.backendTomb[gone] = tomb
	s.backendTombMu.Unlock()
	s.applyBackendRecord(backendRecord{ip: gone, port: 10143, tag: "a", vhosts: 100, up: true})
	if s.ring.GetBackend(gone) != nil {
		t.Fatal("a static snapshot record re-added the backend six minutes after its removal, inside backend_expire")
	}
}

// A handshake carries the lease, so a backend learned that way expires when its
// heartbeats stop instead of living forever as a static entry (#2255).
func TestAHandshakeBackendExpiresWithoutHeartbeats(t *testing.T) {
	s := domainServer(t, alive)
	applyHandshakeHost(s, handshakeHost(gone, uint64(time.Now().Unix())))
	if s.ring.GetBackend(gone) == nil {
		t.Fatal("the handshake did not add the backend")
	}
	s.backendSeenMu.Lock()
	lease, ok := s.backendSeen[gone]
	lease.at = time.Now().Add(-time.Hour)
	s.backendSeen[gone] = lease
	s.backendSeenMu.Unlock()
	if !ok {
		t.Fatal("a backend learned in a handshake carries no lease, so it can never expire")
	}
	s.expireStaleBackends(time.Minute)
	if s.ring.GetBackend(gone) != nil {
		t.Error("a handshake backend with no heartbeat for an hour is still in the ring")
	}
}

// A placement gossiped by a peer that had not yet seen the removal does not pin
// a user or a domain to the backend that left (#2255).
func TestAPlacementOnARemovedBackendIsNotTaken(t *testing.T) {
	s := removed(t, 0, last)
	host := gone + ":10143"
	s.membership.applyEnvelope("DOMAIN-ASSIGN", []string{"one.test", host, "99", "peer"}, "peer", 1)
	s.membership.applyEnvelope("USER-ASSIGN", []string{fmt.Sprint(HashUsername("u1@one.test", s.userDir.hf)), host, "99", "peer"}, "peer", 2)
	if e := s.domainDir.Get("one.test"); e != nil {
		t.Errorf("a gossiped placement put the domain on the removed backend: %+v", *e)
	}
	if e := s.userDir.Get("u1@one.test"); e != nil {
		t.Errorf("a gossiped placement pinned the user to the removed backend: %+v", *e)
	}
}

// Every removal tells the ring which heartbeats came before it: the down it
// gossips carries the tombstone's seq (#2255).
func TestTheRemovalGossipsItsSeq(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove func(s *Server)
	}{
		{"BACKEND-DOWN with the sidecar's seq", func(s *Server) {
			s.handleBackendDown(&client{conn: &captureConn{}}, []string{"BACKEND-DOWN", gone, fmt.Sprint(last)})
		}},
		{"lease expiry, the lease's seq", func(s *Server) {
			s.recordBackendSeen(gone, last)
			s.backendSeenMu.Lock()
			s.backendSeen[gone] = backendLease{seq: last, at: time.Now().Add(-time.Hour)}
			s.backendSeenMu.Unlock()
			s.expireStaleBackends(time.Minute)
		}},
		{"eviction, the lease's seq", func(s *Server) {
			s.recordBackendSeen(gone, last)
			s.evictUnreachable(gone, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := domainServer(t, gone, alive)
			watcher := &captureConn{}
			s.addClient(&client{conn: watcher})
			tc.remove(s)
			if want := fmt.Sprintf("RING-CHANGE\t%s\tdown\ta\t%d", gone, last); !strings.Contains(string(watcher.written), want) {
				t.Errorf("the removal gossiped %q, want a line %q", watcher.written, want)
			}
		})
	}
}
