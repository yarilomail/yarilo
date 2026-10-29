package lockstest

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/locks"
)

// Server runs the lock service on a unix socket for the test and returns a
// dialer: each call is one more process's client, closed with the test.
func Server(t testing.TB) func() locks.Locker {
	t.Helper()
	// A short directory: a socket path under the test's TempDir can pass the
	// unix limit on macOS.
	dir, err := os.MkdirTemp("", "ls")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "l.sock")
	backend := locks.NewMemoryBackend()
	srv := locks.NewServer(backend, slog.New(slog.DiscardHandler), nil)
	ln, err := locks.ListenUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { srv.Close(); cancel(); <-done; _ = backend.Close() })
	return func() locks.Locker {
		c, err := locks.NewClient(context.Background(), locks.DialUnix(sock))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
}
