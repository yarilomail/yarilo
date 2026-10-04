package locks

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"
)

// joinAndVanish takes a place in the line over a raw connection and drops the
// connection without ever leaving it — a session whose client is gone.
func joinAndVanish(t *testing.T, addr, resource string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if werr := writeFields(conn, cmdVersion, protocolVersion); werr != nil {
		t.Fatalf("version: %v", werr)
	}
	br := bufio.NewReader(conn)
	if _, rerr := br.ReadString('\n'); rerr != nil {
		t.Fatalf("version reply: %v", rerr)
	}
	// Milliseconds on the wire. A wait long enough that the ticket outlives the
	// run if nothing removes it when the connection goes.
	if werr := writeFields(conn, cmdLockWait, resource, Owner("u@x.com", "ghost"), "60000", "expunge", "60000"); werr != nil {
		t.Fatalf("lock-wait: %v", werr)
	}
	// Refuse the answer rather than the request: the server must see the line
	// go, not a reply it could not write.
	time.Sleep(200 * time.Millisecond)
	_ = conn.Close()
}

// A contender whose connection is gone must not hold the line: its place lives
// as long as the connection that took it (#1824).
func TestAVanishedContenderDoesNotHoldTheLine(t *testing.T) {
	backend := NewMemoryBackend()
	t.Cleanup(func() { _ = backend.Close() })
	addr, client := standAddr(t, backend)

	const (
		contenders = 20
		hold       = 10 * time.Millisecond
		resource   = "mailbox/u@x.com/INBOX"
	)

	// Someone holds it, so the ghost queues rather than being granted at once.
	holdCtx := WithSite(context.Background(), "expunge")
	held, herr := Acquire(holdCtx, client, resource, Owner("u@x.com", "holder"), 5*time.Second)
	if herr != nil {
		t.Fatal(herr)
	}
	joinAndVanish(t, addr, resource)
	if uerr := client.Unlock(holdCtx, held.ID); uerr != nil {
		t.Fatal(uerr)
	}

	ctx, cancel := context.WithTimeout(WithSite(context.Background(), "expunge"), 90*time.Second)
	defer cancel()

	var mu sync.Mutex
	var longest time.Duration
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started := time.Now()
			lk, err := Acquire(ctx, client, resource, Owner("u@x.com", fmt.Sprintf("s%d", i)), 30*time.Second)
			waited := time.Since(started)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			mu.Lock()
			if waited > longest {
				longest = waited
			}
			mu.Unlock()
			time.Sleep(hold)
			if uerr := client.Unlock(ctx, lk.ID); uerr != nil {
				t.Errorf("unlock: %v", uerr)
			}
		}()
	}
	wg.Wait()

	t.Logf("longest wait behind a vanished contender: %v", longest)
	ceiling := time.Duration(contenders)*hold + 2*time.Second
	if longest > ceiling {
		t.Errorf("the longest wait was %v, want at most %v — the line waited for the ticket to age out", longest, ceiling)
	}
}

// standAddr is one server over backend, returning its address and a client.
func standAddr(t *testing.T, backend Backend) (string, *Client) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := NewServer(backend, testLogger(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		srv.Close()
		<-done
	})
	c, err := NewClient(context.Background(), DialTCP(ln.Addr().String()))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return ln.Addr().String(), c
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
