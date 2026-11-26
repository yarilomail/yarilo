package locks

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func probeServer(t *testing.T) (addr string, logs *logBuf, m *Metrics) {
	t.Helper()
	logs = &logBuf{}
	m = NewMetrics(prometheus.NewRegistry(), "test")
	b := NewMemoryBackend()
	t.Cleanup(func() { _ = b.Close() })
	srv := NewServer(b, slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), m)
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx, ln) }()
	t.Cleanup(func() { cancel(); srv.Close(); <-done })
	return ln.Addr().String(), logs, m
}

// send dials, writes what, closes, and waits until the server has logged.
func send(t *testing.T, addr, what string, logs *logBuf, want string) {
	t.Helper()
	c, err := new(net.Dialer).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if what != "" {
		if _, err := c.Write([]byte(what)); err != nil {
			t.Fatal(err)
		}
	}
	_ = c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(logs.String(), want) {
		time.Sleep(10 * time.Millisecond)
	}
}

// A connection closed before its first byte is a probe, not a failed
// handshake: Debug and a count, never a WARN (#2715).
func TestAConnectionClosedBeforeItsFirstByteIsAProbe(t *testing.T) {
	addr, logs, m := probeServer(t)
	send(t, addr, "", logs, "closed before its first byte")
	out := logs.String()
	if !strings.Contains(out, `"level":"DEBUG","msg":"locks: connection closed before its first byte"`) {
		t.Errorf("no Debug line for a silent connection: %s", out)
	}
	if strings.Contains(out, "handshake failed") {
		t.Errorf("a silent connection was logged as a failed handshake: %s", out)
	}
	if got := counterNow(t, m.silentConns); got != 1 {
		t.Errorf("silent connections = %v, want 1", got)
	}
}

// Anything that sent a byte and is not this protocol's version is still a
// WARN: garbage, another version, and half a version line cut off.
func TestAHandshakeThatSentSomethingWrongIsAWarning(t *testing.T) {
	for name, what := range map[string]string{
		"garbage":         "GET / HTTP/1.1\r\n",
		"another version": cmdVersion + "\t0\n",
		"half a line":     cmdVersion,
	} {
		t.Run(name, func(t *testing.T) {
			addr, logs, m := probeServer(t)
			send(t, addr, what, logs, "handshake failed")
			out := logs.String()
			if !strings.Contains(out, `"level":"WARN","msg":"locks: handshake failed"`) {
				t.Errorf("%s was not a WARN: %s", name, out)
			}
			if got := counterNow(t, m.silentConns); got != 0 {
				t.Errorf("%s counted as a silent connection", name)
			}
		})
	}
}
