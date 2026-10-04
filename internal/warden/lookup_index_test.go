package warden

import (
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newIndexBackend(t *testing.T, ttl time.Duration) (*miniredis.Miniredis, *redis.Client, StateBackend) {
	t.Helper()
	mr := miniredis.RunT(t)
	mr.SetTime(time.Unix(1_790_000_000, 0))
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return mr, rdb, NewRedisBackend(rdb, "test:warden:", "test:warden:events:", time.Minute, ttl, 0)
}

// LOOKUP is one round trip whatever else Redis holds (#2149).
func TestRedisLookupIgnoresOtherUsers(t *testing.T) {
	mr, _, b := newIndexBackend(t, time.Minute)
	for i := 0; i < 1000; i++ {
		if ok, err := b.SessionConnect(fmt.Sprintf("o%d", i), fmt.Sprintf("other%d@x", i), "10.0.0.1", "imap"); !ok || err != nil {
			t.Fatalf("connect other %d: %v %v", i, ok, err)
		}
	}
	for i, svc := range []string{"lmtp", "LMTP", "imap"} {
		if ok, err := b.SessionConnect(fmt.Sprintf("a%d", i), "alice@x", "10.0.0.2", svc); !ok || err != nil {
			t.Fatalf("connect alice %d: %v %v", i, ok, err)
		}
	}
	cases := []struct {
		service string
		want    int
	}{{"lmtp", 2}, {"LMTP", 2}, {"imap", 1}, {"", 3}, {"pop3", 0}}
	for _, c := range cases {
		before := mr.CommandCount()
		if got := b.SessionLookupCount("alice@x", c.service); got != c.want {
			t.Errorf("LOOKUP alice %q = %d, want %d", c.service, got, c.want)
		}
		// One script: its own commands count here, a SCAN over the others would not fit.
		if n := mr.CommandCount() - before; n > 8 {
			t.Errorf("LOOKUP alice %q ran %d commands with 1000 other sessions in Redis", c.service, n)
		}
	}
}

// A session that expired without DISCONNECT is not counted, and its index
// entry goes; one renewed by a heartbeat stays.
func TestRedisLookupDropsExpiredSessions(t *testing.T) {
	const ttl = 10 * time.Second
	mr, rdb, b := newIndexBackend(t, ttl)
	for _, id := range []string{"gone", "kept"} {
		if ok, err := b.SessionConnect(id, "bob@x", "10.0.0.3", "lmtp"); !ok || err != nil {
			t.Fatalf("connect %s: %v %v", id, ok, err)
		}
	}
	now := time.Unix(1_790_000_000, 0)
	step := func(d time.Duration) { now = now.Add(d); mr.SetTime(now); mr.FastForward(d) }
	step(6 * time.Second)
	if !b.SessionTouch("kept") {
		t.Fatal("touch kept: session missing")
	}
	step(6 * time.Second)
	if got := b.SessionLookupCount("bob@x", "lmtp"); got != 1 {
		t.Errorf("LOOKUP after one expiry = %d, want 1", got)
	}
	members, err := rdb.ZRange(t.Context(), "test:warden:usess:bob@x", 0, -1).Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0] != "lmtp\tkept" {
		t.Errorf("index = %q, want only the renewed session", members)
	}
}
