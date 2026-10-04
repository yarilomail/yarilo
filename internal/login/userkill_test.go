package login

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// holdThenHostDirector replies FAIL reason=killing for the first `holds` LOOKUPs
// and HOST afterwards — modelling a confirmed kick that clears mid-retry.
func holdThenHostDirector(t *testing.T, backendAddr string, holds int32) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var seen int32
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
				for {
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
					f := strings.Split(strings.TrimRight(line, "\n"), "\t")
					if f[0] != "LOOKUP" {
						continue
					}
					id := f[1]
					if atomic.AddInt32(&seen, 1) <= holds {
						fmt.Fprintf(conn, "FAIL\t%s\treason=killing\n", id)
						continue
					}
					host, port, _ := net.SplitHostPort(backendAddr)
					fmt.Fprintf(conn, "HOST\t%s\t%s\t%s\n", id, host, port)
				}
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// TestDirectorLookupWithHold_RetriesThenSucceeds: a held LOOKUP (reason=killing)
// is retried, not surfaced as an error, and resolves once the kill confirms.
func TestDirectorLookupWithHold_RetriesThenSucceeds(t *testing.T) {
	dir := holdThenHostDirector(t, "10.0.0.5:993", 2) // hold twice, then HOST
	s := &Server{opts: testOpts(Options{Protocol: ProtocolIMAP, DirectorAddr: dir, LocalIP: "127.0.0.1",
		LookupHoldMax: 5, LookupHoldBackoff: time.Millisecond})}

	addr, err := s.directorLookupWithHold("u@example.com", "imap", slog.Default())
	if err != nil {
		t.Fatalf("a held-then-cleared lookup must succeed after retries, got %v", err)
	}
	if addr != "10.0.0.5:993" {
		t.Errorf("addr = %q, want 10.0.0.5:993", addr)
	}
}

// TestDirectorLookupWithHold_GivesUpAfterBudget: a kill that never clears within
// the retry budget eventually returns an error (the client then reconnects; by
// then the director's hard timeout has cleared the hold).
func TestDirectorLookupWithHold_GivesUpAfterBudget(t *testing.T) {
	dir := holdThenHostDirector(t, "10.0.0.5:993", 1000) // always hold
	s := &Server{opts: testOpts(Options{Protocol: ProtocolIMAP, DirectorAddr: dir, LocalIP: "127.0.0.1",
		LookupHoldMax: 3, LookupHoldBackoff: time.Millisecond})}

	if _, err := s.directorLookupWithHold("u@example.com", "imap", slog.Default()); err == nil {
		t.Fatal("a lookup held past the retry budget must return an error, not spin forever")
	}
}

// Holds and backoff are taken as given; the #858 budget is the chart's, and
// app/guard checks it.
func TestLookupHoldBudget_TakenAsGiven(t *testing.T) {
	zero := &Server{opts: Options{}}
	if got := zero.maxLookupHolds(); got != 0 {
		t.Errorf("maxLookupHolds = %d, want 0", got)
	}
	if got := zero.lookupHoldBackoff(); got != 0 {
		t.Errorf("lookupHoldBackoff = %v, want 0", got)
	}
	over := &Server{opts: testOpts(Options{LookupHoldMax: 7, LookupHoldBackoff: 40 * time.Millisecond})}
	if got := over.maxLookupHolds(); got != 7 {
		t.Errorf("override maxLookupHolds = %d, want 7", got)
	}
	if got := over.lookupHoldBackoff(); got != 40*time.Millisecond {
		t.Errorf("override lookupHoldBackoff = %v, want 40ms", got)
	}
}
