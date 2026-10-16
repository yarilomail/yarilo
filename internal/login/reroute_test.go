package login

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	authclient "github.com/yarilomail/yarilo/internal/auth/client"
)

// rerouteStubDirector is a director stub whose LOOKUP reply is supplied by
// lookupAddr() (so a test can return the same dead pod or switch to a live one)
// and which counts BACKEND-UNREACHABLE reports.
func rerouteStubDirector(t *testing.T, lookupAddr func() string, reports *int32) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				rd := bufio.NewReader(conn)
				fmt.Fprintf(conn, "VERSION\tyarilo-director\t1\t0\n")
				fmt.Fprintf(conn, "DONE\n")
				for { // consume client handshake
					line, err := rd.ReadString('\n')
					if err != nil {
						return
					}
					if strings.TrimRight(line, "\n") == "DONE" {
						break
					}
				}
				for {
					line, err := rd.ReadString('\n')
					if err != nil {
						return
					}
					fields := strings.Split(strings.TrimRight(line, "\n"), "\t")
					switch fields[0] {
					case "LOOKUP":
						host, port, _ := net.SplitHostPort(lookupAddr())
						fmt.Fprintf(conn, "HOST\t%s\t%s\t%s\n", fields[1], host, port)
					case "BACKEND-UNREACHABLE":
						atomic.AddInt32(reports, 1)
						fmt.Fprintf(conn, "OK\n")
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// deadLoopbackAddr returns a loopback address with nothing listening.
func deadLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// TestReroute_SameBackendDoesNotLoop is the guard from the #782 review: when the
// re-LOOKUP returns the SAME still-dead pod, the proxy must report once and give
// up with an error — never spin.
func TestReroute_SameBackendDoesNotLoop(t *testing.T) {
	dead := deadLoopbackAddr(t)
	var reports int32
	dir := rerouteStubDirector(t, func() string { return dead }, &reports)

	s := &Server{opts: testOpts(Options{Protocol: ProtocolIMAP, DirectorAddr: dir, LocalIP: "127.0.0.1"})}
	conn, _, err := s.dialBackendWithReroute("u@example.com", "imap", dead, time.Now().Add(5*time.Second), slog.Default())
	if err == nil {
		if conn != nil {
			conn.Close()
		}
		t.Fatal("dialing a dead backend whose re-LOOKUP returns the same addr must fail, not succeed")
	}
	if got := atomic.LoadInt32(&reports); got != 1 {
		t.Errorf("must report unreachable exactly once (no spin), got %d reports", got)
	}
}

// TestReroute_ToLiveBackendSucceeds: a dead initial backend, then a re-LOOKUP
// yielding a different LIVE pod, must connect after one unreachable report.
func TestReroute_ToLiveBackendSucceeds(t *testing.T) {
	dead := deadLoopbackAddr(t)

	live, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { live.Close() })
	go func() {
		for {
			c, err := live.Accept()
			if err != nil {
				return
			}
			_ = c // hold open; a successful TCP dial is all dialBackend needs
		}
	}()
	liveAddr := live.Addr().String()

	var reports int32
	dir := rerouteStubDirector(t, func() string { return liveAddr }, &reports)

	s := &Server{opts: testOpts(Options{Protocol: ProtocolIMAP, DirectorAddr: dir, LocalIP: "127.0.0.1"})}
	conn, addr, err := s.dialBackendWithReroute("u@example.com", "imap", dead, time.Now().Add(5*time.Second), slog.Default())
	if err != nil {
		t.Fatalf("re-route to a live backend must succeed, got %v", err)
	}
	defer conn.Close()
	if addr != liveAddr {
		t.Errorf("connected addr = %q, want the re-looked-up live addr %q", addr, liveAddr)
	}
	if got := atomic.LoadInt32(&reports); got != 1 {
		t.Errorf("expected exactly one unreachable report before the successful re-route, got %d", got)
	}
}

// blackholing stands in a pod address that drops packets: the dial to it waits
// out whatever deadline it is given, as a removed pod's address does.
func blackholing(t *testing.T, dead string) {
	t.Helper()
	real := dialBackendFn
	t.Cleanup(func() { dialBackendFn = real })
	dialBackendFn = func(addr string, tlsCfg *tls.Config, deadline time.Time) (net.Conn, error) {
		if addr == dead {
			time.Sleep(time.Until(deadline))
			return nil, fmt.Errorf("dial %s: i/o timeout", addr)
		}
		return real(addr, tlsCfg, deadline)
	}
}

// A dead pod address that drops packets must not use up the one deadline: the
// re-route to the live backend gets its share and connects (#2255, #2124).
func TestReroute_ABlackholedAddressLeavesTheRerouteItsShare(t *testing.T) {
	dead := "10.255.255.1:10143"
	blackholing(t, dead)
	live, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { live.Close() })
	go func() {
		for {
			if _, err := live.Accept(); err != nil {
				return
			}
		}
	}()
	var reports int32
	dir := rerouteStubDirector(t, func() string { return live.Addr().String() }, &reports)
	s := &Server{opts: testOpts(Options{Protocol: ProtocolIMAP, DirectorAddr: dir, LocalIP: "127.0.0.1"})}

	budget := 2 * time.Second
	start := time.Now()
	conn, addr, err := s.dialBackendWithReroute("u@example.com", "imap", dead, start.Add(budget), slog.Default())
	if err != nil {
		t.Fatalf("the re-route to a live backend failed after %v: %v", time.Since(start).Round(time.Millisecond), err)
	}
	defer conn.Close()
	if addr != live.Addr().String() {
		t.Errorf("connected to %s, want the live %s", addr, live.Addr())
	}
	if took := time.Since(start); took > budget {
		t.Errorf("took %v, past the one deadline of %v", took, budget)
	}
}

// A failed re-route answers with the address it tried last, so the retry and
// the log name the backend the director now gives, not the dead one (#2255).
func TestReroute_AFailedRerouteNamesTheNewAddress(t *testing.T) {
	dead, rerouted := "10.255.255.1:10143", "10.255.255.2:10143"
	blackholing(t, dead)
	real := dialBackendFn
	dialBackendFn = func(addr string, tlsCfg *tls.Config, deadline time.Time) (net.Conn, error) {
		if addr == rerouted {
			return nil, fmt.Errorf("dial %s: connection refused", addr)
		}
		return real(addr, tlsCfg, deadline)
	}
	var reports int32
	dir := rerouteStubDirector(t, func() string { return rerouted }, &reports)
	s := &Server{opts: testOpts(Options{Protocol: ProtocolIMAP, DirectorAddr: dir, LocalIP: "127.0.0.1"})}

	_, addr, err := s.openBackendSession(&preamble{}, &authclient.AuthResult{}, "u@example.com", "imap", dead, "", "", time.Now().Add(time.Second), slog.Default())
	if err == nil {
		t.Fatal("both backends refuse, and the session opened")
	}
	if addr != rerouted {
		t.Errorf("the failed session names %s, want the re-routed %s", addr, rerouted)
	}
}
