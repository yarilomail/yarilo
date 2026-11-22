package mailboxbase

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// WalkBytesPerFile is what a whole-folder walk allocates per file of the
// folder: 787 B at 2000 files and 681 B at 10 000 on maildir, measured by
// TestAWalkCostsWhatTheBudgetWeighs, which goes red when a walk costs more.
const WalkBytesPerFile = 800

// walkBudget bounds the memory the walks of this process hold together; a walk
// weighs its folder's messages times WalkBytesPerFile, known before it starts.
var walkBudget = newBudget(0)

// SetWalkBudget sets that bound in bytes; zero leaves walks unbounded. Called
// once at start, from the configuration.
func SetWalkBudget(bytes int64) { walkBudget = newBudget(bytes) }

var metricWalkWait = promauto.NewHistogram(prometheus.HistogramOpts{
	Name:    "mailbox_sync_walk_budget_wait_seconds",
	Help:    "Time a whole-folder walk waited for room in the process's walk memory budget.",
	Buckets: prometheus.ExponentialBuckets(0.001, 4, 10),
})

type budget struct {
	mu    sync.Mutex
	cond  *sync.Cond
	limit int64
	used  int64
}

func newBudget(limit int64) *budget {
	b := &budget{limit: limit}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// acquire waits for room for weight bytes and returns its release. A walk
// heavier than the whole budget takes all of it, and so runs alone.
func (b *budget) acquire(weight int64) func() {
	if b.limit <= 0 {
		return func() {}
	}
	need := min(max(weight, 1), b.limit)
	start := time.Now()
	b.mu.Lock()
	waited := false
	for b.used+need > b.limit {
		waited = true
		b.cond.Wait()
	}
	b.used += need
	b.mu.Unlock()
	if waited {
		metricWalkWait.Observe(time.Since(start).Seconds())
	}
	return func() {
		b.mu.Lock()
		b.used -= need
		b.mu.Unlock()
		b.cond.Broadcast()
	}
}

// walkFlights lets one walk run per folder in this process; the sessions that
// arrive during it wait for it and then decide again, on its result.
var walkFlights = &flights{running: map[string]chan struct{}{}}

type flights struct {
	mu      sync.Mutex
	running map[string]chan struct{}
}

// lead makes the caller the folder's walker, or returns the walk to wait for.
func (f *flights) lead(key string) (done func(), wait <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch, ok := f.running[key]; ok {
		return nil, ch
	}
	ch := make(chan struct{})
	f.running[key] = ch
	return func() {
		f.mu.Lock()
		delete(f.running, key)
		f.mu.Unlock()
		close(ch)
	}, nil
}
