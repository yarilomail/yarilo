package locks_test

import (
	"context"
	"log/slog"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/yarilomail/yarilo/v2/pkg/locks"
)

// publishTrips counts the round trips to Redis that carry a PUBLISH.
type publishTrips struct{ n atomic.Int64 }

func (h *publishTrips) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *publishTrips) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "publish" {
			h.n.Add(1)
		}
		return next(ctx, cmd)
	}
}

func (h *publishTrips) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, c := range cmds {
			if c.Name() == "publish" {
				h.n.Add(1)
				break
			}
		}
		return next(ctx, cmds)
	}
}

// An EMIT on an account's key reaches the key's and the account's channels in
// one trip to Redis: the writer waits for the reply, so a second trip is its cost (#2404).
func TestEmitOnAnAccountKeyIsOneTripToRedis(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	trips := &publishTrips{}
	rdb.AddHook(trips)
	backend := locks.NewRedisBackend(rdb)
	defer func() { _ = backend.Close() }()
	srv := locks.NewServer(backend, slog.New(slog.NewTextHandler(os.Stderr, nil)), newTestMetrics(t, "remote"))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ctx, ln)
		close(done)
	}()
	defer func() {
		srv.Close()
		cancel()
		<-done
	}()
	addr := ln.Addr().String()
	if !waitDial(t, "tcp", addr) {
		t.Fatal("server did not start")
	}
	client, err := locks.NewClient(ctx, func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close() }()

	const key = "mbox:alice@example.com:INBOX"
	onKey, err := client.Subscribe(ctx, key)
	if err != nil {
		t.Fatalf("subscribe %s: %v", key, err)
	}
	onAccount, err := client.Subscribe(ctx, locks.AccountKey("alice@example.com"))
	if err != nil {
		t.Fatalf("subscribe account: %v", err)
	}

	trips.n.Store(0)
	if err := client.Emit(ctx, key, locks.EventChanged, "p"); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if got := trips.n.Load(); got != 1 {
		t.Errorf("one EMIT took %d trips to Redis, want 1", got)
	}
	for name, ch := range map[string]<-chan locks.Event{"key": onKey, "account": onAccount} {
		select {
		case e := <-ch:
			if e.Resource != key || e.Payload != "p" {
				t.Errorf("%s channel got %+v, want resource %s payload p", name, e, key)
			}
		case <-ctx.Done():
			t.Fatalf("%s channel heard nothing", name)
		}
	}
}
