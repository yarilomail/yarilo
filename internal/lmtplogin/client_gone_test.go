package lmtplogin

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
)

// gatedBackend speaks one LMTP transaction per connection and counts what it
// saves; the first delivery waits on gate.
type gatedBackend struct {
	gate  chan struct{}
	first sync.Once
	mu    sync.Mutex
	saved int
}

func (b *gatedBackend) count() int { b.mu.Lock(); defer b.mu.Unlock(); return b.saved }

func newGatedBackend(t *testing.T, gated bool) (*gatedBackend, string) {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	b := &gatedBackend{gate: make(chan struct{})}
	if !gated {
		close(b.gate)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go b.serve(c)
		}
	}()
	return b, ln.Addr().String()
}

func (b *gatedBackend) serve(c net.Conn) {
	defer c.Close()
	rd := bufio.NewReader(c)
	if _, err := rd.ReadString('\n'); err != nil { // preamble
		return
	}
	fmt.Fprint(c, "220 backend\r\n")
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		switch strings.ToUpper(strings.Fields(line)[0]) {
		case "DATA":
			fmt.Fprint(c, "354 go\r\n")
			for {
				l, err := rd.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
			}
			b.first.Do(func() { <-b.gate })
			b.mu.Lock()
			b.saved++
			b.mu.Unlock()
			fmt.Fprint(c, "250 2.0.0 Saved\r\n")
		default:
			fmt.Fprint(c, "250 OK\r\n")
		}
	}
}

// A client that pipelines a batch and leaves without reading the replies gets
// at most the message in flight delivered, not the rest of the batch (#2615).
func TestASessionWhoseClientLeftStops(t *testing.T) {
	const batch = 20
	for _, tc := range []struct {
		name      string
		leave     bool
		wantAtMax int
		wantAtMin int
	}{
		{"a client that reads every reply gets the whole batch", false, batch, batch},
		{"a client that leaves gets at most the message in flight", true, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ended := make(chan struct{})
			sessionEnded = func() { close(ended) }
			t.Cleanup(func() { sessionEnded = func() {} })

			be, backendAddr := newGatedBackend(t, tc.leave)
			proxyAddr := startLMTPLogin(t, testOpts(Options{
				Hostname: "test.local", BackendAddr: backendAddr, AuthMasterAddr: startTestAuth(t),
				WardenAddr: startTestWarden(t), ConcurrencyLimit: -1,
			}))
			mta := dialMTA(t, proxyAddr)
			mta.readCode(t, 220)

			var b strings.Builder
			b.WriteString("LHLO mta.test\r\n")
			for i := 1; i <= batch; i++ {
				fmt.Fprintf(&b, "MAIL FROM:<a@external.test>\r\nRCPT TO:<alice@example.com>\r\nDATA\r\nSubject: %d\r\n\r\nbody\r\n.\r\n", i)
			}
			b.WriteString("QUIT\r\n")
			if _, err := mta.conn.Write([]byte(b.String())); err != nil {
				t.Fatal(err)
			}
			if tc.leave {
				// Unread replies make the close a reset, as a sender that gave up does.
				mta.conn.Close()
				close(be.gate)
			} else {
				for {
					line, err := mta.rd.ReadString('\n')
					if err != nil || strings.HasPrefix(line, "221") {
						break
					}
				}
				mta.conn.Close()
			}
			<-ended
			if got := be.count(); got > tc.wantAtMax || got < tc.wantAtMin {
				t.Fatalf("backend saved %d, want %d..%d", got, tc.wantAtMin, tc.wantAtMax)
			}
		})
	}
}
