package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
)

// fakePOP3 is a maildrop that answers correctly unless a test bends one answer.
// Without it the row would only prove the check runs, not that it judges.
type fakePOP3 struct {
	mu   sync.Mutex
	ln   net.Listener
	body string
	uidl string
	// The bends, one per row.
	statCount    int  // 0 = the truth
	retrShort    bool // hand back fewer octets than LIST promised
	duplicateUID bool // give two messages one id
	dropOnAbort  bool // remove the message for a session that never quit
	keepOnQuit   bool // keep it after a DELE the session committed
	deleted      bool // committed state
	oneAtATime   bool // refuse a second session, as a locked maildrop does
	open         int  // sessions currently on the wire
	overlapped   bool // two were open at once
}

func newFakePOP3(t *testing.T, marker string) *fakePOP3 {
	t.Helper()
	f := &fakePOP3{
		body: "Subject: yarilo smoke pop3 " + marker + "\r\n\r\nthe pop3 probe body\r\n",
		uidl: "probe-uidl-1",
	}
	_, cert, _, _ := issueMTLSFixtures(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	f.ln = ln
	t.Cleanup(func() { ln.Close() }) //nolint:errcheck
	go f.serve()
	return f
}

func (f *fakePOP3) port() string {
	_, p, _ := net.SplitHostPort(f.ln.Addr().String())
	return p
}

func (f *fakePOP3) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.session(c)
	}
}

// enter and leave count the sessions on the maildrop, so an overlap is caught
// here rather than by a server refusing it in production (#1734).
func (f *fakePOP3) enter() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.open > 0 {
		f.overlapped = true
		if f.oneAtATime {
			return false
		}
	}
	f.open++
	return true
}

func (f *fakePOP3) leave() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.open--
}

func (f *fakePOP3) sawOverlap() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.overlapped
}

func (f *fakePOP3) has() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.deleted
}

func (f *fakePOP3) setDeleted() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = true
}

func (f *fakePOP3) session(c net.Conn) {
	defer c.Close() //nolint:errcheck
	r := bufio.NewReader(c)
	pending := false
	if !f.enter() {
		fmt.Fprintf(c, "+OK fake ready\r\n")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(strings.ToUpper(line), "USER") || strings.HasPrefix(strings.ToUpper(line), "PASS") {
				fmt.Fprintf(c, "+OK\r\n")
				continue
			}
			fmt.Fprintf(c, "-ERR mailbox already in use, try again later\r\n")
		}
	}
	defer f.leave()
	fmt.Fprintf(c, "+OK fake ready\r\n")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			// Dropped without QUIT: nothing this session marked is committed,
			// unless the test asks the server to lose it anyway.
			if pending && f.dropOnAbort {
				f.setDeleted()
			}
			return
		}
		cmd := strings.ToUpper(strings.Fields(strings.TrimSpace(line))[0])
		switch cmd {
		case "USER", "PASS", "NOOP":
			fmt.Fprintf(c, "+OK\r\n")
		case "STAT":
			n, octets := f.count(), int64(len(f.body))
			if !f.has() {
				octets = 0
			}
			if f.statCount != 0 {
				n = f.statCount
			}
			fmt.Fprintf(c, "+OK %d %d\r\n", n, octets)
		case "LIST":
			fmt.Fprintf(c, "+OK\r\n")
			if f.has() {
				fmt.Fprintf(c, "1 %d\r\n", len(f.body))
			}
			fmt.Fprintf(c, ".\r\n")
		case "UIDL":
			fmt.Fprintf(c, "+OK\r\n")
			if f.has() {
				fmt.Fprintf(c, "1 %s\r\n", f.uidl)
				if f.duplicateUID {
					fmt.Fprintf(c, "2 %s\r\n", f.uidl)
				}
			}
			fmt.Fprintf(c, ".\r\n")
		case "TOP":
			fmt.Fprintf(c, "+OK\r\n")
			if f.has() {
				headers, _, _ := strings.Cut(f.body, "\r\n\r\n")
				fmt.Fprintf(c, "%s\r\n", headers)
			}
			fmt.Fprintf(c, ".\r\n")
		case "RETR":
			if !f.has() {
				fmt.Fprintf(c, "-ERR no such message\r\n")
				continue
			}
			out := f.body
			if f.retrShort {
				// Whole and well-framed, just shorter than LIST promised.
				headers, _, _ := strings.Cut(f.body, "\r\n\r\n")
				out = headers + "\r\n\r\n"
			}
			fmt.Fprintf(c, "+OK\r\n%s.\r\n", out)
		case "DELE":
			pending = true
			fmt.Fprintf(c, "+OK\r\n")
		case "QUIT":
			if pending && !f.keepOnQuit {
				f.setDeleted()
			}
			fmt.Fprintf(c, "+OK bye\r\n")
			return
		default:
			fmt.Fprintf(c, "-ERR unknown\r\n")
		}
	}
}

func (f *fakePOP3) count() int {
	if f.has() {
		return 1
	}
	return 0
}

// runCycleAgainst points the check at the fake and returns what it decided.
func runCycleAgainst(t *testing.T, f *fakePOP3, marker string) error {
	t.Helper()
	restore := func(host, port string, insecure bool) func() {
		oh, op, oi := *flagPOP3Host, *flagPOP3SPort, *flagInsecure
		*flagPOP3Host, *flagPOP3SPort, *flagInsecure = host, port, insecure
		return func() { *flagPOP3Host, *flagPOP3SPort, *flagInsecure = oh, op, oi }
	}("127.0.0.1", f.port(), true)
	defer restore()

	s, err := pop3Login("u", "p")
	if err != nil {
		t.Fatalf("login against the fake: %v", err)
	}
	num, listed, err := s.findProbe(marker)
	if err != nil {
		s.close()
		t.Fatalf("the fake does not hold the probe: %v", err)
	}
	return pop3Cycle(s, "u", "p", marker, num, listed)
}

// The maildrop a client can trust: the numbers agree with each other and with
// what RETR delivers, ids are unique, and a deletion is final only after QUIT.
func TestThePOP3CycleAcceptsAnHonestMaildrop(t *testing.T) {
	marker := "probe-marker"
	f := newFakePOP3(t, marker)
	if err := runCycleAgainst(t, f, marker); err != nil {
		t.Errorf("an honest maildrop was refused: %v", err)
	}
	if f.has() {
		t.Error("the probe was left in the maildrop; the cycle does not clean up after itself")
	}
}

// Each bend is a way a maildrop lies that a client would act on: a wrong count,
// a truncated fetch, a shared id, a deletion that fires or does not.
func TestThePOP3CycleCatchesAMaildropThatLies(t *testing.T) {
	for _, tc := range []struct {
		name string
		bend func(*fakePOP3)
		want string
	}{
		{"STAT counts what LIST does not", func(f *fakePOP3) { f.statCount = 3 }, "STAT counts 3 messages, LIST names 1"},
		{"RETR is shorter than LIST promised", func(f *fakePOP3) { f.retrShort = true }, "RETR delivered"},
		{"two messages share one uidl", func(f *fakePOP3) { f.duplicateUID = true }, "share the uidl"},
		{"a session that never quit still deleted", func(f *fakePOP3) { f.dropOnAbort = true }, "a DELE it did not commit removed it"},
		{"a committed deletion did nothing", func(f *fakePOP3) { f.keepOnQuit = true }, "still in the maildrop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := "probe-marker"
			f := newFakePOP3(t, marker)
			tc.bend(f)
			err := runCycleAgainst(t, f, marker)
			if err == nil {
				t.Fatalf("the cycle accepted a maildrop that %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the cycle said %q, which does not name %q", err, tc.want)
			}
		})
	}
}

// The STAT total is the sum of the rows, not a number of its own.
func TestThePOP3CycleReadsSTATAsTwoNumbers(t *testing.T) {
	f := newFakePOP3(t, "probe-marker")
	restore := func() func() {
		oh, op, oi := *flagPOP3Host, *flagPOP3SPort, *flagInsecure
		*flagPOP3Host, *flagPOP3SPort, *flagInsecure = "127.0.0.1", f.port(), true
		return func() { *flagPOP3Host, *flagPOP3SPort, *flagInsecure = oh, op, oi }
	}()
	defer restore()
	s, err := pop3Login("u", "p")
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	count, octets, err := s.stat()
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || octets != int64(len(f.body)) {
		t.Errorf("STAT read as %d/%d, want 1/%d", count, octets, len(f.body))
	}
}

// The check keeps one session on the maildrop: a server holding it for its own
// session refuses the second, and the check must not be what opens it (#1734).
func TestThePOP3CycleKeepsOneSessionAtATime(t *testing.T) {
	marker := "probe-marker"
	f := newFakePOP3(t, marker)
	f.oneAtATime = true
	if err := runCycleAgainst(t, f, marker); err != nil {
		t.Errorf("a maildrop serving one session at a time refused the check: %v", err)
	}
	if f.sawOverlap() {
		t.Error("the check had two sessions open on one maildrop")
	}
}
