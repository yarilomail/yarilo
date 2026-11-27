package locks

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
)

// InstrumentRedis times every command the lock service sends to its Redis
// (Valkey) on r: yarilo_locks_backend_seconds by command, a pipeline as one
// observation. Its count over yarilo_locks_acquire_seconds' count is how many
// round trips one lock costs; its sum is how much of a client's exchange the
// store takes (#2591).
func InstrumentRedis(rdb redis.UniversalClient, r prometheus.Registerer) {
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "yarilo_locks_backend_seconds",
		Help:    "Duration of each command the lock service sends to its Redis, by command; a pipeline counts once as \"pipeline\".",
		Buckets: prometheus.ExponentialBuckets(0.00005, 2, 14), // 50µs … ~400ms
	}, []string{"command"})
	r.MustRegister(h)
	rdb.AddHook(backendTimer{h: h})
}

type backendTimer struct{ h *prometheus.HistogramVec }

func (t backendTimer) DialHook(next redis.DialHook) redis.DialHook { return next }

func (t backendTimer) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmd)
		t.h.WithLabelValues(cmd.Name()).Observe(time.Since(start).Seconds())
		return err
	}
}

func (t backendTimer) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmds)
		t.h.WithLabelValues("pipeline").Observe(time.Since(start).Seconds())
		return err
	}
}
