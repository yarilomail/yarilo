package stopaccept

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

// A stopping backend refuses a new session at once, so the login proxy reroutes
// it, and keeps the sessions it has so they drain. The stand's stopping backend
// took a session and answered nothing for 15 s (#2260).
func TestAStoppingBackendRefusesNewSessions(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	exited := make(chan int, 1)
	exit = func(code int) { exited <- code }
	t.Cleanup(func() { exit = os.Exit })
	held := make(chan net.Conn, 4)
	returned := make(chan struct{})
	serve := func(l net.Listener) error {
		defer close(returned)
		for {
			c, err := l.Accept()
			if err != nil {
				return err
			}
			held <- c
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	Serve(ctx, "test", ln, serve)

	open, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	server := <-held

	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			break
		}
		c.Close()
		if time.Now().After(deadline) {
			t.Fatal("a stopping backend still accepts new sessions two seconds after the signal")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := server.Write([]byte("* OK still here\r\n")); err != nil {
		t.Errorf("the session opened before the signal was cut: %v", err)
	}
	<-returned
	select {
	case code := <-exited:
		t.Errorf("the listener closed for the stop was taken as a failure: exit(%d)", code)
	case <-time.After(100 * time.Millisecond):
	}
}

// A listener that stops serving before any signal is a pod that cannot serve:
// that is still fatal.
func TestAServeThatEndsUnaskedIsFatal(t *testing.T) {
	exited := make(chan int, 1)
	exit = func(code int) { exited <- code }
	t.Cleanup(func() { exit = os.Exit })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	Serve(context.Background(), "test", ln, func(net.Listener) error { return errors.New("broken") })
	select {
	case code := <-exited:
		if code != 1 {
			t.Errorf("exit(%d), want 1", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a listener that stopped serving on its own did not end the process")
	}
}
