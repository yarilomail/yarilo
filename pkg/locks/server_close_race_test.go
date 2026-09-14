package locks

import (
	"context"
	"net"
	"testing"
	"time"
)

// Once Close has begun, no further handler is registered (#1795): the panic
// needs the Add to land inside Wait's return, which cannot be scheduled.
func TestNoHandlerIsRegisteredOnceCloseBegan(t *testing.T) {
	s := NewServer(NewMemoryBackend(), nil, nil)
	if !s.addHandler() {
		t.Fatal("a fresh server refused to register a handler")
	}
	s.wg.Done()

	s.Close()
	if s.addHandler() {
		s.wg.Done()
		t.Error("a handler was registered after Close began waiting")
	}
}

// And the accept loop honours that: a connection arriving as Close runs is
// closed rather than served, and Serve returns.
func TestServeReturnsWhenCloseRacesAnAccept(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(NewMemoryBackend(), nil, nil)

	t.Cleanup(SetTestBeforeHandlerAdd(func() {
		done := make(chan struct{})
		go func() { defer close(done); s.Close() }()
		<-done
	}))

	served := make(chan error, 1)
	go func() { served <- s.Serve(context.Background(), ln) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn.Close() //nolint:errcheck

	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after Close")
	}
}
