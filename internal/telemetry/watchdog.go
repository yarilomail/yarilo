package telemetry

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// LivenessCheck is a cheap local self-check; it must not touch a shared
// dependency, or one Redis hiccup restarts the whole tier.
type LivenessCheck func(context.Context) error

// WatchdogOptions configures the timer-driven liveness watchdog. It is opt-in:
// with no Check the watchdog never runs and /healthz stays unconditional.
type WatchdogOptions struct {
	// Check is the self-check. Nil disables the watchdog entirely.
	Check LivenessCheck
	// Interval is the gap between checks; timer-driven so idle components are not
	// restarted for lack of traffic.
	Interval time.Duration
	// Timeout bounds a single check and MUST be shorter than Interval; a check that
	// exceeds it counts as failed.
	Timeout time.Duration
	// FailureThreshold is how many CONSECUTIVE failed checks trip /healthz.
	FailureThreshold int
}

// watchdog runs the self-check on a timer and records whether /healthz should
// currently fail.
type watchdog struct {
	check     LivenessCheck
	interval  time.Duration
	timeout   time.Duration
	threshold int

	fails   atomic.Int64
	tripped atomic.Bool
}

// newWatchdog returns a watchdog for opts, or nil when no Check is set; defaults
// fill in any non-positive tunable.
func newWatchdog(opts WatchdogOptions) *watchdog {
	if opts.Check == nil {
		return nil
	}
	w := &watchdog{
		check:     opts.Check,
		interval:  opts.Interval,
		timeout:   opts.Timeout,
		threshold: opts.FailureThreshold,
	}
	return w
}

// run ticks until ctx is done. Each tick runs the check under its own timeout in
// a separate goroutine, so a check that ignores cancellation cannot stall the loop.
func (w *watchdog) run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

func (w *watchdog) tick(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.check(cctx) }()

	var err error
	select {
	case err = <-done:
	case <-cctx.Done():
		err = cctx.Err()
	}

	if err == nil {
		w.fails.Store(0)
		if w.tripped.Swap(false) {
			slog.Info("telemetry: liveness watchdog recovered")
		}
		return
	}
	n := w.fails.Add(1)
	if int(n) >= w.threshold && !w.tripped.Swap(true) {
		slog.Error("telemetry: liveness watchdog tripped — /healthz will fail so the container restarts",
			"consecutive_failures", n, "err", err)
	}
}

// unhealthy reports whether the watchdog has tripped and /healthz should fail.
func (w *watchdog) unhealthy() bool { return w != nil && w.tripped.Load() }
