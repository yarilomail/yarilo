package redis

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/yarilomail/yarilo/v2/pkg/dict"
	"github.com/yarilomail/yarilo/v2/pkg/dict/dicttest"
)

func TestContractSuite(t *testing.T) {
	dicttest.RunSuite(t, func(t *testing.T) dict.Dict {
		mr := miniredis.RunT(t)
		d, err := New(dict.Config{Settings: map[string]any{"addr": mr.Addr()}})
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		return d
	})
}

func TestPrefixIsolation(t *testing.T) {
	mr := miniredis.RunT(t)
	a, _ := New(dict.Config{Settings: map[string]any{"addr": mr.Addr(), "prefix": "ns-a:"}})
	b, _ := New(dict.Config{Settings: map[string]any{"addr": mr.Addr(), "prefix": "ns-b:"}})
	defer a.Close()
	defer b.Close()

	tx, _ := a.Begin(context.Background(), nil)
	_ = tx.Set("k", []byte("a"))
	_, _ = tx.Commit()

	_, found, _ := b.Lookup(context.Background(), nil, "k")
	if found {
		t.Error("prefix isolation broken: ns-b sees ns-a's keys")
	}
	_, foundA, _ := a.Lookup(context.Background(), nil, "k")
	if !foundA {
		t.Error("ns-a lost its own key")
	}
}

func TestTTLViaExpireSecs(t *testing.T) {
	mr := miniredis.RunT(t)
	d, _ := New(dict.Config{Settings: map[string]any{"addr": mr.Addr()}})
	defer d.Close()

	tx, _ := d.Begin(context.Background(), &dict.OpSettings{ExpireSecs: 60})
	_ = tx.Set("ttl-key", []byte("v"))
	_, _ = tx.Commit()

	// Verify Redis side sees the TTL set.
	ttl := mr.TTL("ttl-key")
	if ttl <= 0 || ttl > 60*time.Second {
		t.Errorf("expected TTL ~60s, got %v", ttl)
	}

	mr.FastForward(61 * time.Second)
	_, found, _ := d.Lookup(context.Background(), nil, "ttl-key")
	if found {
		t.Error("expired key still visible after fast-forward")
	}
}

func TestMissingAddrErrors(t *testing.T) {
	if _, err := New(dict.Config{}); err == nil {
		t.Error("missing addr should error")
	}
}

func TestRegisteredAtInit(t *testing.T) {
	for _, n := range dict.Drivers() {
		if n == "redis" {
			return
		}
	}
	t.Errorf("redis driver not registered: %v", dict.Drivers())
}

func openIndexed(t *testing.T, mr *miniredis.Miniredis, prefix string) *Dict {
	t.Helper()
	d, err := New(dict.Config{Settings: map[string]any{"addr": mr.Addr(), "prefix": prefix}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d.(*Dict)
}

func setKeys(t *testing.T, d dict.Dict, set *dict.OpSettings, kv ...string) {
	t.Helper()
	tx, _ := d.Begin(context.Background(), set)
	for i := 0; i+1 < len(kv); i += 2 {
		_ = tx.Set(kv[i], []byte(kv[i+1]))
	}
	if res, err := tx.Commit(); res != dict.CommitOK || err != nil {
		t.Fatalf("commit: %v %v", res, err)
	}
}

func listed(t *testing.T, d dict.Dict, path string) []string {
	t.Helper()
	it, err := d.Iterate(context.Background(), nil, path, dict.IterRecurse|dict.IterNoValue|dict.IterSortByKey)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for it.Next() {
		keys = append(keys, it.Key())
	}
	return keys
}

// A prefix read costs what it returns, not the db's size: the db is shared with
// locks and counters, and SCAN walks all of it (#2387).
func TestIterateCostsWhatItReturns(t *testing.T) {
	mr := miniredis.RunT(t)
	for i := 0; i < 5000; i++ {
		mr.Set(fmt.Sprintf("locks:%d", i), "x")
	}
	d := openIndexed(t, mr, "acl:")
	setKeys(t, d, nil, "shared/anyone/alice", "1", "shared/anyone/bob", "1")
	listed(t, d, "shared/anyone/")

	before := mr.CommandCount()
	got := listed(t, d, "shared/anyone/")
	if n := mr.CommandCount() - before; n > 5 {
		t.Errorf("iterating 2 keys in a db of 5002 took %d commands", n)
	}
	if strings.Join(got, ",") != "shared/anyone/alice,shared/anyone/bob" {
		t.Errorf("listed %v", got)
	}
}

// Keys written before the index existed are listed: the first iterate builds it.
func TestKeysFromBeforeTheIndexAreListed(t *testing.T) {
	mr := miniredis.RunT(t)
	mr.Set("acl:shared/anyone/alice", "1")
	mr.Set("acl:shared/user/bob/carol", "1")
	mr.Set("other:shared/anyone/zed", "1")
	d := openIndexed(t, mr, "acl:")
	if got := listed(t, d, "shared/"); strings.Join(got, ",") != "shared/anyone/alice,shared/user/bob/carol" {
		t.Errorf("listed %v, want the two keys under acl:", got)
	}
	setKeys(t, d, nil, "shared/anyone/dave", "1")
	if got := listed(t, d, "shared/anyone/"); strings.Join(got, ",") != "shared/anyone/alice,shared/anyone/dave" {
		t.Errorf("after a write: listed %v", got)
	}
}

// An unset leaves the index, and so does a key deleted behind the dict's back.
func TestGoneKeysLeaveTheIndex(t *testing.T) {
	mr := miniredis.RunT(t)
	d := openIndexed(t, mr, "acl:")
	setKeys(t, d, nil, "a/1", "1", "a/2", "1", "a/3", "1")
	tx, _ := d.Begin(context.Background(), nil)
	_ = tx.Unset("a/1")
	_, _ = tx.Commit()
	mr.Del("acl:a/2")
	if got := listed(t, d, "a/"); strings.Join(got, ",") != "a/3" {
		t.Errorf("listed %v, want a/3", got)
	}
	if members, _ := mr.ZMembers(d.idxKey()); strings.Join(members, ",") != "acl:a/3" {
		t.Errorf("index holds %v, want acl:a/3", members)
	}
}

// Expiring keys leave the index with the writes that make more of them, so a
// dict nobody iterates does not keep every key it ever had.
func TestExpiredKeysLeaveTheIndex(t *testing.T) {
	mr := miniredis.RunT(t)
	d := openIndexed(t, mr, "dup:")
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return now }
	ttl := &dict.OpSettings{ExpireSecs: 60}
	setKeys(t, d, ttl, "x/1", "1", "x/2", "1")
	mr.FastForward(61 * time.Second)
	now = now.Add(61 * time.Second)
	setKeys(t, d, ttl, "x/3", "1")
	if members, _ := mr.ZMembers(d.idxKey()); strings.Join(members, ",") != "dup:x/3" {
		t.Errorf("index holds %v after the first two expired, want dup:x/3", members)
	}
	if members, _ := mr.ZMembers(d.expKey()); strings.Join(members, ",") != "dup:x/3" {
		t.Errorf("expiry index holds %v, want dup:x/3", members)
	}
}

// A key an older pod wrote without its index entry, after the index was built,
// is listed once a process starting later has rebuilt the index.
func TestARestartPicksUpKeysWrittenWithoutTheIndex(t *testing.T) {
	mr := miniredis.RunT(t)
	first := openIndexed(t, mr, "acl:")
	setKeys(t, first, nil, "shared/anyone/alice", "1")
	listed(t, first, "shared/")
	mr.Set("acl:shared/anyone/bob", "1") // an older pod: no index entry

	next := openIndexed(t, mr, "acl:")
	listed(t, next, "shared/")
	select {
	case <-next.rebuildDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the rebuild of a starting process never finished")
	}
	if got := listed(t, next, "shared/anyone/"); strings.Join(got, ",") != "shared/anyone/alice,shared/anyone/bob" {
		t.Errorf("after the restart rebuild: listed %v", got)
	}
	listed(t, next, "shared/")
	if n := next.builds.Load(); n != 1 {
		t.Errorf("a process rebuilt the index %d times over three iterates, want once", n)
	}
}

// A build costs a fixed number of commands per SCAN batch, not one per
// expiring key: a dict of sieve duplicates holds thousands of them.
func TestABuildOfManyExpiringKeysIsBatched(t *testing.T) {
	mr := miniredis.RunT(t)
	for i := 0; i < 2000; i++ {
		k := fmt.Sprintf("dup:x/%d", i)
		mr.Set(k, "1")
		mr.SetTTL(k, time.Hour)
	}
	d := openIndexed(t, mr, "dup:")
	trips := &roundTrips{}
	d.client.AddHook(trips)
	if err := d.build(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := trips.n.Load(); n > 10 {
		t.Errorf("building an index of 2000 expiring keys took %d round trips", n)
	}
	if members, _ := mr.ZMembers(d.expKey()); len(members) != 2000 {
		t.Errorf("expiry index holds %d keys, want 2000", len(members))
	}
}

// roundTrips counts the requests a client sends: a command or a pipeline each.
type roundTrips struct{ n atomic.Int32 }

func (r *roundTrips) DialHook(next goredis.DialHook) goredis.DialHook { return next }
func (r *roundTrips) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error { r.n.Add(1); return next(ctx, cmd) }
}
func (r *roundTrips) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error { r.n.Add(1); return next(ctx, cmds) }
}
