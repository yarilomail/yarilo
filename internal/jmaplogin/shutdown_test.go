package jmaplogin

import (
	"context"
	"net"
	"testing"
	"time"
)

// A connection that has sent no request, such as the spare one a client's
// pool dials, does not hold the shutdown: net/http alone would wait 5 s on it
// (#2462).
func TestAConnectionWithoutARequestDoesNotHoldShutdown(t *testing.T) {
	probe, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close() //nolint:errcheck

	s := New(Options{Addr: addr, Router: StaticRouter{Addr: "127.0.0.1:1"}, Auth: fakeAuth{}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()

	var idle net.Conn
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if idle, err = (&net.Dialer{}).DialContext(t.Context(), "tcp", addr); err == nil {
			break
		}
	}
	if idle == nil {
		t.Fatalf("proxy at %s never accepted: %v", addr, err)
	}
	defer idle.Close()                //nolint:errcheck
	time.Sleep(50 * time.Millisecond) // accepted and in StateNew

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve = %v", err)
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("Serve took %v to return", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return within 3 s with an idle connection open")
	}
}
