package imap

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeWarden answers SELECT like the service does, and records what arrived.
type fakeWarden struct {
	ln net.Listener

	mu     sync.Mutex
	events []string

	answer  atomic.Bool // false: read the line and stay silent
	blocked atomic.Bool // true: accept but never read
}

func startFakeWarden(t *testing.T) *fakeWarden {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	w := &fakeWarden{ln: ln}
	w.answer.Store(true)
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go w.serve(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return w
}

func (w *fakeWarden) serve(conn net.Conn) {
	defer conn.Close() //nolint:errcheck
	// The greeting the real service sends, both lines; the client reads them
	// on dial and refuses the connection without them.
	fmt.Fprint(conn, "VERSION\tyarilo-warden\t1\t0\nDONE\n")
	rd := bufio.NewReader(conn)
	for {
		if w.blocked.Load() {
			// Accepted and never read: the socket buffer is the only thing
			// between the writer and a stall.
			time.Sleep(50 * time.Millisecond)
			continue
		}
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		fields := strings.Split(strings.TrimRight(line, "\n"), "\t")
		if len(fields) >= 2 && fields[0] == "SELECT" {
			if len(fields) == 2 {
				fields = append(fields, "")
			}
			w.mu.Lock()
			w.events = append(w.events, fields[1]+" "+fields[2])
			w.mu.Unlock()
			if w.answer.Load() {
				// Padded on purpose: the property is that unread answers fill
				// the socket buffer and stop the writer, and short answers
				// would need tens of thousands of events to show it.
				fmt.Fprintf(conn, "OK\t%s\t%s\n", fields[1], strings.Repeat("x", 32768))
			}
		}
	}
}

func (w *fakeWarden) seen() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.events...)
}

func (w *fakeWarden) waitFor(t *testing.T, n int, within time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		got := w.seen()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("the warden received %d events in %s, want %d", len(got), within, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// SELECT hands the event over and returns: the command's goroutine does not
// wait for the warden, however slow it is (#1875).
func TestSelectDoesNotWaitForTheWarden(t *testing.T) {
	w := startFakeWarden(t)
	w.blocked.Store(true) // a warden that never reads
	c := newImapWardenClient(w.ln.Addr().String(), nil, 64)
	defer c.Close()

	start := time.Now()
	for i := 0; i < 50; i++ {
		c.PushSelect("sess", "INBOX")
	}
	if took := time.Since(start); took > 200*time.Millisecond {
		t.Errorf("fifty pushes took %s on the command's goroutine", took)
	}
}

// The events reach the warden, in the order they were pushed for one session.
func TestEventsReachTheWardenInOrder(t *testing.T) {
	w := startFakeWarden(t)
	c := newImapWardenClient(w.ln.Addr().String(), nil, 64)
	defer c.Close()

	c.PushSelect("sess", "INBOX")
	c.PushSelect("sess", "Sent")
	c.PushSelect("sess", "") // UNSELECT

	got := w.waitFor(t, 3, 5*time.Second)
	want := []string{"sess INBOX", "sess Sent", "sess "}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d is %q, want %q (order matters for one session)", i, got[i], want[i])
		}
	}
}

// A warden that is not there does not fail SELECT: the push returns and the
// session carries on.
func TestAnAbsentWardenDoesNotFailSelect(t *testing.T) {
	c := newImapWardenClient("127.0.0.1:1", nil, 8) // nobody listens
	defer c.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			c.PushSelect("sess", "INBOX")
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pushing to an absent warden blocked the caller")
	}
}

// A full queue drops the oldest rather than blocking: the newest event is the
// one that describes the session now.
func TestAFullQueueDropsRatherThanBlocks(t *testing.T) {
	w := startFakeWarden(t)
	w.blocked.Store(true)
	c := newImapWardenClient(w.ln.Addr().String(), nil, 4)
	defer c.Close()

	start := time.Now()
	for i := 0; i < 500; i++ {
		c.PushSelect("sess", fmt.Sprintf("folder-%d", i))
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("five hundred pushes into a queue of four took %s; it blocked", took)
	}
}

// The writer must read the answers back. A warden that answers every event
// fills the socket buffer if nobody drains it, and the writer stops with a
// queue behind it -- which is the whole point of reading and discarding.
func TestTheWriterKeepsDeliveringWhenTheWardenAnswers(t *testing.T) {
	w := startFakeWarden(t)
	c := newImapWardenClient(w.ln.Addr().String(), nil, 4096)
	defer c.Close()

	const n = 400
	for i := 0; i < n; i++ {
		c.PushSelect("sess", fmt.Sprintf("folder-%d", i))
	}
	w.waitFor(t, n, 15*time.Second)
}
