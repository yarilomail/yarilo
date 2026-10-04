package warden_test

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/warden"
)

// A cancelled context ends a dial whose greeting never comes, well before the
// dial's own timeout (#2059).
func TestDialContextEndsAWaitForTheGreeting(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan struct{})
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		close(accepted)
		_, _ = io.Copy(io.Discard, conn) // accepted, never greeted
		_ = conn.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() {
		c, derr := warden.DialContext(ctx, ln.Addr().String(), nil, 10*time.Second)
		if c != nil {
			c.Close()
		}
		res <- derr
	}()
	<-accepted
	cancel()
	select {
	case derr := <-res:
		if !errors.Is(derr, context.Canceled) {
			t.Errorf("dial ended with %v, want the cancel", derr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the dial outlived its context by 3s")
	}
}
