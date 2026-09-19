package locks_test

import (
	"context"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/locks"
)

// memoryFactoryRestartable is memoryFactory with a way to stop the server and
// start a new one on the same socket, which is what a pod restart looks like
// to a client holding connections.
func memoryFactoryRestartable(t *testing.T) (locks.Locker, func(), func()) {
	t.Helper()
	socket := shortSocketPath(t)

	type running struct {
		srv     *locks.Server
		backend *locks.MemoryBackend
		cancel  context.CancelFunc
		done    chan struct{}
	}
	var cur running

	start := func() {
		backend := locks.NewMemoryBackend(locks.WithSweepInterval(10 * time.Millisecond))
		srv := locks.NewServer(backend, slog.New(slog.NewTextHandler(os.Stderr, nil)), newTestMetrics(t, "embedded"))
		ln, err := locks.ListenUnix(socket)
		if err != nil {
			t.Fatalf("listen unix: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			_ = srv.Serve(ctx, ln)
			close(done)
		}()
		if !waitDial(t, "unix", socket) {
			t.Fatal("server did not start")
		}
		cur = running{srv: srv, backend: backend, cancel: cancel, done: done}
	}
	stop := func() {
		cur.srv.Close()
		cur.cancel()
		_ = cur.backend.Close()
		<-cur.done
		_ = os.Remove(socket)
	}

	start()
	client, err := locks.NewClient(context.Background(), locks.DialUnix(socket))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	restart := func() {
		stop()
		start()
	}
	cleanup := func() {
		_ = client.Close()
		stop()
	}
	return client, cleanup, restart
}

var _ = net.Dial
