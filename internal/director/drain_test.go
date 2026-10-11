package director

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/backendreg"
	"github.com/yarilomail/yarilo/internal/cluster/ring"
)

// A drained director answers nothing more: an open connection is closed, not
// served from the view it left with, and a new one gets no handshake (#2152).
func TestDrainStopsServing(t *testing.T) {
	srv, addr := startServer(t)
	srv.ring.AddBackend(&ring.Backend{IP: "10.0.0.1", Port: 993, Up: true, Vhosts: 100})

	conn, sc := dialTest(t, addr)
	readHandshake(t, sc)
	sendHandshake(t, conn)
	conn.Write([]byte("LOOKUP\t1\tuser@example.com\t\n"))
	if line := readLine(t, sc); !strings.HasPrefix(line, "HOST\t1\t10.0.0.1") {
		t.Fatalf("before drain: %q, want a HOST", line)
	}

	srv.Drain()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn.Write([]byte("LOOKUP\t2\tuser@example.com\t\n"))
	if sc.Scan() {
		t.Errorf("after drain: answered %q, want the connection closed", sc.Text())
	}

	fresh, fsc := dialTest(t, addr)
	_ = fresh.SetReadDeadline(time.Now().Add(2 * time.Second))
	if fsc.Scan() {
		t.Errorf("new connection after drain: got %q, want it closed", fsc.Text())
	}
}

// A backend registered on a director that drains reconnects within its
// backoff and reaches the next one, not when the old process exits.
func TestDrainedDirectorBackendReconnects(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	serve := func(ln net.Listener) (*Server, context.CancelFunc) {
		srv := NewWithOptions(testOptions(Options{PingInterval: 24 * time.Hour}))
		ctx, cancel := context.WithCancel(context.Background())
		go func() { _ = srv.listenOn(ctx, ln) }()
		return srv, cancel
	}
	srvA, stopA := serve(ln)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg := backendreg.New(backendreg.Options{
		DirectorAddr: addr, SelfIP: "10.0.0.7", Port: 993, Vhosts: 100, Interval: 200 * time.Millisecond,
	})
	go reg.Run(ctx)
	waitFor(t, 3*time.Second, func() bool { return srvA.ring.GetBackend("10.0.0.7") != nil })

	srvA.Drain()
	stopA()
	ln.Close()
	var lnB net.Listener
	waitFor(t, 2*time.Second, func() bool {
		lnB, err = net.Listen("tcp", addr)
		return err == nil
	})
	srvB, stopB := serve(lnB)
	defer stopB()
	waitFor(t, 4*time.Second, func() bool { return srvB.ring.GetBackend("10.0.0.7") != nil })
}
