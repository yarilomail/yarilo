package dictsrv

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/pkg/dict"
	"github.com/yarilomail/yarilo/v2/pkg/dict/proxy"
)

func counterOf(t *testing.T, d dict.Dict) dict.Counter {
	t.Helper()
	c, ok := d.(dict.Counter)
	if !ok {
		t.Fatalf("%T is no dict.Counter", d)
	}
	return c
}

// OpIncr through the service: the key is made on the first call, each call
// returns the sum, the service's prefix and the user's scope are applied by
// the service, and the lifetime is the one asked for.
func TestIncrThroughTheProxy(t *testing.T) {
	ctx := context.Background()
	d, _, mr := serveRedis(t)
	c := counterOf(t, d)
	set := &dict.OpSettings{Username: "u1@d.test"}
	for want := int64(1); want <= 3; want++ {
		got, err := c.Incr(ctx, set, "priv/sieve/send-rate/2026111810", 1, 2*time.Hour)
		if err != nil || got != want {
			t.Fatalf("incr %d: got %d, %v", want, got, err)
		}
	}
	if got, err := c.Incr(ctx, set, "priv/sieve/send-rate/2026111810", 20, 2*time.Hour); err != nil || got != 23 {
		t.Fatalf("incr by 20: got %d, %v", got, err)
	}
	key := onlyKey(t, mr, "send-rate")
	if !strings.HasPrefix(key, "t:") || !strings.Contains(key, "u1@d.test") {
		t.Fatalf("key %q: want the service's prefix t: and the user's scope", key)
	}
	if ttl := mr.TTL(key); ttl != 2*time.Hour {
		t.Fatalf("ttl %v, want 2h", ttl)
	}
	other, err := c.Incr(ctx, &dict.OpSettings{Username: "u2@d.test"}, "priv/sieve/send-rate/2026111810", 1, 2*time.Hour)
	if err != nil || other != 1 {
		t.Fatalf("another user's counter: %d, %v, want 1", other, err)
	}
}

// Concurrent increments from several clients each get a distinct sum: the
// operation is one step on the server, so no two callers see the same value.
func TestIncrIsOneStepAcrossClients(t *testing.T) {
	ctx := context.Background()
	_, addr, _ := serveRedis(t)
	const clients, each = 4, 25
	seen := make(chan int64, clients*each)
	var wg sync.WaitGroup
	for i := 0; i < clients; i++ {
		c := proxy.New(addr, "d", nil)
		t.Cleanup(func() { _ = c.Close() })
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				n, err := c.Incr(ctx, &dict.OpSettings{Username: "u1@d.test"}, "k", 1, time.Hour)
				if err != nil {
					t.Error(err)
					return
				}
				seen <- n
			}
		}()
	}
	wg.Wait()
	close(seen)
	got := map[int64]bool{}
	for n := range seen {
		if got[n] {
			t.Fatalf("value %d returned twice", n)
		}
		got[n] = true
	}
	if len(got) != clients*each {
		t.Fatalf("%d distinct values, want %d", len(got), clients*each)
	}
}

// A dict whose driver cannot count answers F with the dict and driver named,
// which the client reads as dict.ErrNoCounter.
func TestIncrOnADriverWithoutCounter(t *testing.T) {
	c, _ := serveOne(t, "plain")
	_, err := counterOf(t, c).Incr(context.Background(), &dict.OpSettings{Username: "u1@d.test"}, "k", 1, time.Hour)
	if !errors.Is(err, dict.ErrNoCounter) || !strings.Contains(err.Error(), "driver memory") {
		t.Fatalf("err %v, want dict.ErrNoCounter naming driver memory", err)
	}
}

// A service that predates OpIncr answers it as an unknown command, as this
// server does any letter it does not know; the client reads it as
// dict.ErrNoCounter rather than a value.
func TestIncrOnAServiceThatPredatesIt(t *testing.T) {
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		rd := bufio.NewReader(conn)
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			if line[0] == proxy.OpHello {
				continue
			}
			_, _ = conn.Write([]byte("F unknown command\n"))
		}
	}()
	c := proxy.New(ln.Addr().String(), "d", nil)
	t.Cleanup(func() { _ = c.Close() })
	_, err = c.Incr(context.Background(), &dict.OpSettings{Username: "u1@d.test"}, "k", 1, time.Hour)
	if !errors.Is(err, dict.ErrNoCounter) || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("err %v, want dict.ErrNoCounter with the service's words", err)
	}
}

// A store the service cannot reach is an ordinary error with the service's
// words, not dict.ErrNoCounter: the dict can count, it just failed this time.
func TestIncrOnAnUnreachableStore(t *testing.T) {
	d, _, mr := serveRedis(t)
	mr.Close()
	_, err := counterOf(t, d).Incr(context.Background(), &dict.OpSettings{Username: "u1@d.test"}, "k", 1, time.Hour)
	if err == nil || errors.Is(err, dict.ErrNoCounter) {
		t.Fatalf("err %v, want an error that is not dict.ErrNoCounter", err)
	}
	if !strings.Contains(err.Error(), "redis incr") {
		t.Fatalf("err %v, want the service's words", err)
	}
}
