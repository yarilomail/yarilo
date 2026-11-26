package locks

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func histLabelled(t *testing.T, v *prometheus.HistogramVec, labels ...string) (float64, uint64) {
	t.Helper()
	h, err := v.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("get %v: %v", labels, err)
	}
	var m dto.Metric
	if err := h.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write %v: %v", labels, err)
	}
	return m.GetHistogram().GetSampleSum(), m.GetHistogram().GetSampleCount()
}

func counterLabelled(t *testing.T, v *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	c, err := v.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("get %v: %v", labels, err)
	}
	var m dto.Metric
	if err := c.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write %v: %v", labels, err)
	}
	return m.GetCounter().GetValue()
}

// busyLocker refuses the first busyFor attempts and then succeeds, which is a
// contender losing that many draws in a row.
type busyLocker struct {
	busyFor int
	calls   int
}

func (b *busyLocker) Lock(context.Context, string, string, time.Duration) (Lock, error) {
	b.calls++
	if b.calls <= b.busyFor {
		return Lock{}, ErrBusy
	}
	return Lock{ID: "l"}, nil
}
func (b *busyLocker) LockShared(ctx context.Context, r, o string, ttl time.Duration) (Lock, error) {
	return b.Lock(WithSite(ctx, "write"), r, o, ttl)
}
func (b *busyLocker) Unlock(context.Context, string) error { return nil }
func (b *busyLocker) Renew(context.Context, string, time.Duration) error {
	return nil
}
func (b *busyLocker) HoldsResource(string) (HoldMode, bool)                   { return HoldNone, false }
func (b *busyLocker) Close() error                                            { return nil }
func (b *busyLocker) Subscribe(context.Context, string) (<-chan Event, error) { return nil, nil }
func (b *busyLocker) Emit(context.Context, string, EventType, string) error   { return nil }
func (b *busyLocker) IncrementCounter(context.Context, string, int64) (int64, error) {
	return 0, nil
}

// One acquisition is one observation, whatever it cost. A contender that lost
// four draws is one slow acquisition, and the wait recorded is the whole wait
// rather than the last sleep (#1640).
func TestAWaitIsMeasuredOncePerAcquisitionNotPerAttempt(t *testing.T) {
	const key = "mbox:u@x/INBOX"
	_, countBefore := histLabelled(t, clientAcquireWait, "mbox")
	sumBefore, _ := histLabelled(t, clientAcquireWait, "mbox")
	attemptSum, attemptCount := histLabelled(t, clientAcquireAttempts, "mbox")

	l := &busyLocker{busyFor: 4}
	if _, err := Acquire(WithSite(context.Background(), "write"), l, key, "test.bin/1/alice@example.com/sess1", time.Second); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	sum, count := histLabelled(t, clientAcquireWait, "mbox")
	if count-countBefore != 1 {
		t.Errorf("the wait was observed %d times for one acquisition, want 1", count-countBefore)
	}
	// Four backoffs of at least 0.75ms each: the wait is the whole wait, and a
	// per-attempt reading would divide it.
	if waited := sum - sumBefore; waited < 0.003 {
		t.Errorf("the acquisition recorded %.4fs of waiting after losing four draws, which is less "+
			"than the backoffs it slept: the clock is not spanning the retries", waited)
	}
	newSum, newCount := histLabelled(t, clientAcquireAttempts, "mbox")
	if newCount-attemptCount != 1 || newSum-attemptSum != 5 {
		t.Errorf("attempts recorded %v observations summing %v, want 1 observation of 5",
			newCount-attemptCount, newSum-attemptSum)
	}
}

// A contender that runs out of time is counted where it happened, with how many
// attempts it had burned. That count is the stall, in the raw. The deadline
// comes from the attempts, not the clock: the locker cancels on its third
// call, so a slow runner does not move the bucket (#2730).
func TestAContenderThatRunsOutOfTimeIsCountedWithItsAttempts(t *testing.T) {
	const key = "mbox:u@x/INBOX"
	buckets := []string{"1", "2-3", "4-7", "8-15", "16-31"}
	before := map[string]float64{}
	for _, b := range buckets {
		before[b] = counterLabelled(t, clientGaveUp, "mbox", b)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	l := &cancellingLocker{cancelOn: 3, cancel: cancel}
	if _, err := Acquire(WithSite(ctx, "write"), l, key, "test.bin/1/alice@example.com/sess1", time.Second); err == nil {
		t.Fatal("the acquisition succeeded against a locker that never yields")
	}
	moved := map[string]float64{}
	for _, b := range buckets {
		if d := counterLabelled(t, clientGaveUp, "mbox", b) - before[b]; d != 0 {
			moved[b] = d
		}
	}
	if len(moved) != 1 || moved["2-3"] != 1 {
		t.Errorf("gave-up counts moved %v after %d attempts, want map[2-3:1] -- the deadline passed during the third attempt", moved, l.calls)
	}
}

// cancellingLocker is always busy and cancels the caller's context on its
// cancelOn-th call, as a deadline that passes during that attempt.
type cancellingLocker struct {
	busyLocker
	cancelOn int
	cancel   context.CancelFunc
	calls    int
}

func (c *cancellingLocker) Lock(context.Context, string, string, time.Duration) (Lock, error) {
	c.calls++
	if c.calls == c.cancelOn {
		c.cancel()
	}
	return Lock{}, ErrBusy
}
func (c *cancellingLocker) LockShared(ctx context.Context, r, o string, ttl time.Duration) (Lock, error) {
	return c.Lock(ctx, r, o, ttl)
}

// Attempts become a bucket, not a number: the label is written by whoever
// stalls, and one series per attempt count is one series per contender.
func TestAttemptsAreBucketed(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{{1, "1"}, {2, "2-3"}, {3, "2-3"}, {4, "4-7"}, {15, "8-15"}, {16, "16-31"}, {64, "64+"}, {5000, "64+"}} {
		if got := attemptBucket(tc.n); got != tc.want {
			t.Errorf("attemptBucket(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
