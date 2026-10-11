package login

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/cluster/proto"
	"github.com/yarilomail/yarilo/internal/director"
)

// When the director a login is watching drains, the login reaches the next one
// and its lookups are answered from that view, not the departed one (#2152).
func TestLookupAfterDirectorDrain(t *testing.T) {
	serve := func(ln net.Listener, backend string) (*director.Server, context.CancelFunc) {
		srv := director.NewWithOptions(director.Options{PingInterval: 24 * time.Hour, PingTimeout: 10 * time.Second,
			UserExpire: 15 * time.Minute, DomainExpire: 15 * time.Minute, DomainRebalanceInterval: time.Minute, UserKillTimeout: 15 * time.Second})
		srv.AddBackend(backend, 993, "", 100)
		ctx, cancel := context.WithCancel(context.Background())
		go func() { _ = srv.Serve(ctx, ln) }()
		return srv, cancel
	}
	lnA, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lnA.Addr().String()
	srvA, stopA := serve(lnA, "10.0.0.1")

	s := New(testOpts(Options{DirectorAddr: addr, LocalIP: "127.0.0.1"}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Watch(ctx)
	watching := func() bool {
		s.watchMu.RLock()
		defer s.watchMu.RUnlock()
		return s.watch != nil
	}
	deadline := time.Now().Add(3 * time.Second)
	for !watching() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got, err := s.directorLookup("u@example.com", ""); err != nil || got != "10.0.0.1:993" {
		t.Fatalf("before drain: %q, %v", got, err)
	}

	srvA.Drain()
	stopA()
	lnA.Close()
	var lnB net.Listener
	for deadline = time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if lnB, err = net.Listen("tcp", addr); err == nil {
			break
		}
	}
	if lnB == nil {
		t.Fatalf("relisten on %s: %v", addr, err)
	}
	_, stopB := serve(lnB, "10.0.0.2")
	defer stopB()

	var got string
	for deadline = time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if got, err = s.directorLookup("u@example.com", ""); err == nil && got == "10.0.0.2:993" && watching() {
			return
		}
	}
	t.Fatalf("after drain: lookup %q (%v), watching=%v; want the next director's 10.0.0.2:993 over a new watch", got, err, watching())
}

// A lookup that starts after the watch connection died is not left waiting for
// a reply that cannot come: it falls back at once (#2152).
func TestLookupOnAClosedWatchFallsBackAtOnce(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("DONE\n"))
		buf := make([]byte, 4096)
		for {
			if _, err := conn.Read(buf); err != nil {
				return
			}
		}
	}()
	c, err := proto.Dial(ln.Addr().String(), "127.0.0.1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	wc := &watchConn{c: c}
	wc.failPending()
	start := time.Now()
	if _, err := wc.lookup("1", "u@example.com", "", "imap", 3*time.Second); !errors.Is(err, errWatchClosed) {
		t.Fatalf("lookup on a closed watch: %v, want errWatchClosed", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("lookup on a closed watch took %s", d)
	}
}
