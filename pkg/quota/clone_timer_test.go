package quota

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/pkg/dict"
)

// countingDict records what the mirror writes, and when.
type countingDict struct {
	dict.Dict
	writes atomic.Int64
	last   atomic.Int64 // storage bytes of the last write
}

func (c *countingDict) Begin(ctx context.Context, set *dict.OpSettings) (dict.Tx, error) {
	tx, err := c.Dict.Begin(ctx, set)
	if err != nil {
		return nil, err
	}
	return &countingTx{Tx: tx, owner: c}, nil
}

type countingTx struct {
	dict.Tx
	owner *countingDict
	val   int64
}

func (t *countingTx) Set(key string, value []byte) error {
	if key == KeyStorage {
		var n int64
		for _, b := range value {
			n = n*10 + int64(b-'0')
		}
		t.val = n
	}
	return t.Tx.Set(key, value)
}

func (t *countingTx) Commit() (dict.CommitResult, error) {
	t.owner.writes.Add(1)
	t.owner.last.Store(t.val)
	return t.Tx.Commit()
}

func cloneWith(t *testing.T, delay time.Duration) (*Clone, *countingDict) {
	t.Helper()
	inner, err := dict.Open(dict.Config{Driver: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	d := &countingDict{Dict: inner}
	return NewClone([]dict.Dict{d}, delay), d
}

// A save does not wait for the mirror: recording a change writes nothing to the
// dict, and the write happens later, off the caller's goroutine (#1875).
func TestASaveDoesNotWaitForTheMirror(t *testing.T) {
	c, d := cloneWith(t, 300*time.Millisecond)
	c.Acquire("u1@d.test")

	start := time.Now()
	c.Mirror("u1@d.test", Usage{StorageBytes: 10, Messages: 1})
	took := time.Since(start)

	if n := d.writes.Load(); n != 0 {
		t.Errorf("the mirror wrote %d times on the caller's path, want 0", n)
	}
	if took > 50*time.Millisecond {
		t.Errorf("recording a change took %s; the caller is waiting for something", took)
	}
}

// The mirror has the value no later than the delay.
func TestTheMirrorGetsTheValueWithinTheDelay(t *testing.T) {
	c, d := cloneWith(t, 200*time.Millisecond)
	c.Acquire("u1@d.test")
	c.Mirror("u1@d.test", Usage{StorageBytes: 42, Messages: 2})

	deadline := time.Now().Add(3 * time.Second)
	for d.writes.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the mirror never wrote")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := d.last.Load(); got != 42 {
		t.Errorf("the mirror holds %d bytes, want 42", got)
	}
}

// Two updates inside one delay are one write, and it carries the later value.
func TestTwoUpdatesInOneDelayAreOneWrite(t *testing.T) {
	c, d := cloneWith(t, 400*time.Millisecond)
	c.Acquire("u1@d.test")
	c.Mirror("u1@d.test", Usage{StorageBytes: 1, Messages: 1})
	c.Mirror("u1@d.test", Usage{StorageBytes: 2, Messages: 2})
	c.Mirror("u1@d.test", Usage{StorageBytes: 3, Messages: 3})

	time.Sleep(900 * time.Millisecond)
	if n := d.writes.Load(); n != 1 {
		t.Errorf("three updates inside one delay produced %d writes, want 1", n)
	}
	if got := d.last.Load(); got != 3 {
		t.Errorf("the write carried %d bytes, want the last value 3", got)
	}
}

// The last session of a user flushes what is pending; an earlier one leaving
// does not write on behalf of the sessions still open.
func TestTheLastSessionFlushes(t *testing.T) {
	c, d := cloneWith(t, time.Hour) // long enough that only Release can write
	c.Acquire("u1@d.test")
	c.Acquire("u1@d.test")
	c.Mirror("u1@d.test", Usage{StorageBytes: 7, Messages: 1})

	c.Release("u1@d.test")
	time.Sleep(50 * time.Millisecond)
	if n := d.writes.Load(); n != 0 {
		t.Errorf("a session left with another still open and the mirror wrote %d times", n)
	}

	c.Release("u1@d.test")
	deadline := time.Now().Add(2 * time.Second)
	for d.writes.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the last session left and nothing was written")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := d.last.Load(); got != 7 {
		t.Errorf("the final write carried %d bytes, want 7", got)
	}
}
