package locks

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
)

// Every command the lock service sends to its Redis is timed by command, so
// a lock's round trips to the store and their time can be read from the
// service's page.
func TestTheLockServiceTimesItsRedisCommands(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	reg := prometheus.NewRegistry()
	InstrumentRedis(rdb, reg)
	b := NewRedisBackend(rdb)
	ctx := WithSite(context.Background(), "test")
	id, _, err := b.Acquire(ctx, "threads:u@example.com", Owner("u@example.com", NewID()), "test", "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Release(ctx, id); err != nil {
		t.Fatal(err)
	}
	n := testutil.CollectAndCount(reg, "yarilo_locks_backend_seconds")
	if n == 0 {
		t.Fatal("no Redis command was timed")
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var calls uint64
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			calls += m.GetHistogram().GetSampleCount()
		}
	}
	if calls < 2 {
		t.Errorf("an acquire and a release timed %d commands, want at least 2", calls)
	}
	t.Logf("one acquire and release: %d Redis commands over %d series", calls, n)
}
