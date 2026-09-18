package dictsrv

import (
	"errors"
	"fmt"
	"time"

	"context"
	"net"
	"testing"

	"github.com/yarilomail/yarilo/pkg/quota"

	"github.com/yarilomail/yarilo/pkg/dict"
	_ "github.com/yarilomail/yarilo/pkg/dict/memory"
	"github.com/yarilomail/yarilo/pkg/dict/proxy"
)

// serveOne starts the service over one named dict and returns a client of it.
func serveOne(t *testing.T, name string) (dict.Dict, dict.Dict) {
	t.Helper()
	real, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatalf("open memory dict: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go New(map[string]dict.Dict{name: real}, nil).Serve(ctx, ln) //nolint:errcheck

	c := proxy.New(ln.Addr().String(), name, nil)
	t.Cleanup(func() { _ = c.Close() })
	return c, real
}

// serveOneLimited is serveOne with the connection ceiling set, so a row can ask
// what one connection costs.
func serveOneLimited(t *testing.T, name string, limit int) (dict.Dict, dict.Dict) {
	t.Helper()
	real, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go New(map[string]dict.Dict{name: real}, nil).Serve(ctx, ln) //nolint:errcheck
	c := proxy.NewWithLimit(ln.Addr().String(), name, nil, limit)
	t.Cleanup(func() { _ = c.Close() })
	return c, real
}

// A proxied dict answers what the engine holds: the session sees one hop more,
// not different data (#1733).
func TestAProxiedDictReadsWhatTheEngineHolds(t *testing.T) {
	c, real := serveOne(t, "metadata")
	ctx := context.Background()
	set := &dict.OpSettings{Username: "u1@d.test"}

	tx, err := real.Begin(ctx, set)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Set("priv/one", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	vals, found, err := c.Lookup(ctx, set, "priv/one")
	if err != nil || !found {
		t.Fatalf("lookup through the proxy: found=%v err=%v", found, err)
	}
	if string(vals[0]) != "first" {
		t.Errorf("read %q, want %q", vals[0], "first")
	}

	if _, found, err := c.Lookup(ctx, set, "priv/missing"); found || err != nil {
		t.Errorf("a missing key answered found=%v err=%v", found, err)
	}
}

// A write through the proxy lands in the engine, and a value carrying a tab or
// a newline survives: the protocol is line-delimited, the data is not.
func TestAProxiedWriteLandsInTheEngine(t *testing.T) {
	c, real := serveOne(t, "metadata")
	ctx := context.Background()
	set := &dict.OpSettings{Username: "u1@d.test"}

	tx, err := c.Begin(ctx, set)
	if err != nil {
		t.Fatal(err)
	}
	awkward := []byte("line\nwith\ttabs\\and a backslash")
	if err := tx.Set("priv/awkward", awkward); err != nil {
		t.Fatal(err)
	}
	if res, err := tx.Commit(); err != nil || res != dict.CommitOK {
		t.Fatalf("commit: %v %v", res, err)
	}

	vals, found, err := real.Lookup(ctx, set, "priv/awkward")
	if err != nil || !found {
		t.Fatalf("the engine does not hold the write: found=%v err=%v", found, err)
	}
	if string(vals[0]) != string(awkward) {
		t.Errorf("the engine holds %q, want %q", vals[0], awkward)
	}
}

// Iterate through the proxy returns the engine's set: the ACL registry reads
// its owners this way, and a missing row hides someone's shared folders (#1733).
func TestIterateThroughTheProxyReturnsTheSameSet(t *testing.T) {
	c, real := serveOne(t, "acl_shared")
	ctx := context.Background()
	set := &dict.OpSettings{Username: "u1@d.test"}

	tx, _ := real.Begin(ctx, set)
	for _, k := range []string{"shared/acl/a", "shared/acl/b", "shared/acl/c", "other/x"} {
		if err := tx.Set(k, []byte("1")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	got := map[string]bool{}
	it, err := c.Iterate(ctx, set, "shared/acl/", dict.IterRecurse)
	if err != nil {
		t.Fatal(err)
	}
	for it.Next() {
		got[it.Key()] = true
	}
	if err := it.Close(); err != nil {
		t.Fatalf("iterate: %v", err)
	}

	want := map[string]bool{"shared/acl/a": true, "shared/acl/b": true, "shared/acl/c": true}
	if len(got) != len(want) {
		t.Fatalf("iterate returned %v, want %v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Errorf("iterate missed %s", k)
		}
	}
}

// The service going away is an error to the caller, not a session that hangs --
// the same rule the auth relay follows (#1733).
func TestAGoneServiceIsAnErrorNotAHang(t *testing.T) {
	c, _ := serveOne(t, "metadata")
	ctx := context.Background()
	set := &dict.OpSettings{Username: "u1@d.test"}
	if _, _, err := c.Lookup(ctx, set, "priv/one"); err != nil {
		t.Fatalf("the first lookup should work: %v", err)
	}

	gone := proxy.New("127.0.0.1:1", "metadata", nil)
	done := make(chan error, 1)
	go func() {
		_, _, err := gone.Lookup(ctx, set, "priv/one")
		done <- err
	}()
	if err := <-done; err == nil {
		t.Error("a dict service that cannot be reached answered without an error")
	}
}

// One failing clone target is a log line, never an error on the save path; the
// proxy moves that failure into another process, and the shape must hold (#1733).
func TestOneFailingCloneTargetDoesNotFailTheSave(t *testing.T) {
	good, _ := serveOne(t, "quota_clone_redis")
	// A dict name the service does not serve: the same shape as an engine that
	// is down, refused at hello rather than at the write.
	bad := proxy.New(unreachableAddr(t), "quota_clone_mysql", nil)
	t.Cleanup(func() { _ = bad.Close() })

	clone := quota.NewClone([]dict.Dict{good, bad})
	done := make(chan struct{})
	go func() {
		clone.Write(context.Background(), "u1@d.test", quota.Usage{StorageBytes: 4242, Messages: 7})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("a failing clone target blocked the save path")
	}

	vals, found, err := good.Lookup(context.Background(), &dict.OpSettings{Username: "u1@d.test"}, quota.KeyStorage)
	if err != nil || !found {
		t.Fatalf("the healthy target did not get the write: found=%v err=%v", found, err)
	}
	if string(vals[0]) != "4242" {
		t.Errorf("the healthy target holds %q, want 4242", vals[0])
	}
}

// unreachableAddr returns an address nothing listens on: a port bound and then
// released, so the dial is refused rather than routed somewhere real.
func unreachableAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// An iteration holds its connection to the terminator, so a lookup from another
// session shares nothing with it. On one connection the lookup waits out the
// whole stream, which is what queued 36 goroutines on the stand (#1902).
func TestALookupDoesNotWaitOutSomeoneElsesIteration(t *testing.T) {
	c, real := serveOne(t, "acl_shared")
	ctx := context.Background()
	set := &dict.OpSettings{Username: "u1@d.test"}

	tx, _ := real.Begin(ctx, set)
	for i := 0; i < 200; i++ {
		if err := tx.Set(fmt.Sprintf("shared/acl/%03d", i), []byte("1")); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Set("priv/one", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	it, err := c.Iterate(ctx, set, "shared/acl/", dict.IterRecurse)
	if err != nil {
		t.Fatal(err)
	}
	if !it.Next() {
		t.Fatal("the iteration yielded nothing")
	}

	// Mid-stream, with the iterator still open and not drained.
	done := make(chan error, 1)
	go func() {
		_, found, lerr := c.Lookup(ctx, set, "priv/one")
		if lerr == nil && !found {
			lerr = errors.New("lookup did not find the key")
		}
		done <- lerr
	}()
	select {
	case lerr := <-done:
		if lerr != nil {
			t.Fatalf("lookup during an iteration: %v", lerr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a lookup waited on someone else's iteration")
	}

	for it.Next() {
	}
	if err := it.Close(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
}

// And the reason the pool exists: on one connection the same lookup waits,
// because iterate streams untagged rows and only COMMIT_ASYNC carries an id
// (INTERNALS §32). This is the protocol, not a defect -- it is why a session
// needs more than one connection (#1902).
func TestOnOneConnectionALookupWaitsForTheIteration(t *testing.T) {
	c, real := serveOneLimited(t, "acl_shared", 1)
	ctx := context.Background()
	set := &dict.OpSettings{Username: "u1@d.test"}

	tx, _ := real.Begin(ctx, set)
	for i := 0; i < 200; i++ {
		if err := tx.Set(fmt.Sprintf("shared/acl/%03d", i), []byte("1")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	it, err := c.Iterate(ctx, set, "shared/acl/", dict.IterRecurse)
	if err != nil {
		t.Fatal(err)
	}
	if !it.Next() {
		t.Fatal("the iteration yielded nothing")
	}

	done := make(chan struct{})
	go func() {
		_, _, _ = c.Lookup(ctx, set, "priv/one")
		close(done)
	}()
	select {
	case <-done:
		t.Error("a lookup answered while an iteration held the only connection")
	case <-time.After(300 * time.Millisecond):
	}

	for it.Next() {
	}
	_ = it.Close()
	<-done
}
