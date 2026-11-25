package locks

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
)

// wakePing is how long the shared subscription may stay silent before it is
// pinged, and how long the ping has to be answered. A connection that died
// without a word would otherwise hold every waiter of the replica to the
// backstop until TCP gives up.
const wakePing = 15 * time.Second

// wakeRetry spaces the attempts to reach the store again after the
// subscription's connection broke.
const wakeRetry = 100 * time.Millisecond

// wakeHub is the one subscription a replica keeps to the release channels of
// every resource. Before it, each waiting acquire opened a connection of its
// own to the store and subscribed on it; on the stand that was four acquires
// in five, about 4 ms of handshake each (#2591).
type wakeHub struct {
	rdb    redis.UniversalClient
	prefix string // release channel prefix; the resource follows it
	ping   time.Duration

	mu      sync.Mutex
	ps      *redis.PubSub
	closed  bool
	waiters map[string]map[*wakeWaiter]struct{} // by resource

	resubscribed prometheus.Counter
}

type wakeWaiter struct {
	ticket string
	out    chan struct{}
}

func newWakeHub(rdb redis.UniversalClient, prefix string) *wakeHub {
	return &wakeHub{
		rdb:     rdb,
		prefix:  prefix,
		ping:    wakePing,
		waiters: make(map[string]map[*wakeWaiter]struct{}),
		resubscribed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "yarilo_locks_wake_resubscribed_total",
			Help: "Times the lock service's shared release subscription was established again after its connection broke; every waiter of the replica is woken each time.",
		}),
	}
}

// start subscribes once and keeps the subscription for the life of the
// backend. A failed first attempt is returned and tried again by the next
// caller.
func (h *wakeHub) start(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return redis.ErrClosed
	}
	if h.ps != nil {
		return nil
	}
	ps, err := h.subscribe(ctx)
	if err != nil {
		return err
	}
	h.ps = ps
	go h.run(ps)
	return nil
}

func (h *wakeHub) subscribe(ctx context.Context) (*redis.PubSub, error) {
	ps := h.rdb.PSubscribe(ctx, globEscape(h.prefix)+"*")
	if _, err := ps.Receive(ctx); err != nil {
		_ = ps.Close()
		return nil, fmt.Errorf("locks/redis: wakes: %w", err)
	}
	return ps, nil
}

// run reads the subscription until the backend closes. When the connection
// breaks, or stays silent through a ping and the wait for its answer, the
// subscription is dropped and taken again on a fresh connection. A release
// published in between reached nobody, so every waiter here is then woken to
// look for itself.
func (h *wakeHub) run(ps *redis.PubSub) {
	ctx := context.Background()
	pinged := false
	for {
		msg, err := ps.ReceiveTimeout(ctx, h.ping)
		if err == nil {
			pinged = false // any word from the store answers the ping
			if m, ok := msg.(*redis.Message); ok {
				h.deliver(strings.TrimPrefix(m.Channel, h.prefix), m.Payload)
			}
			continue
		}
		if errors.Is(err, redis.ErrClosed) || h.isClosed() {
			return
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() && !pinged {
			// Silence. The client keeps a connection a read timed out on, so
			// only an answer to the ping says it is still there.
			pinged = true
			if perr := ps.Ping(ctx); perr == nil {
				continue
			}
		}
		if ps = h.resubscribe(ctx, ps); ps == nil {
			return
		}
		pinged = false
	}
}

// resubscribe drops the subscription's connection and takes the subscription
// again until it holds or the backend closes, then wakes every waiter.
func (h *wakeHub) resubscribe(ctx context.Context, old *redis.PubSub) *redis.PubSub {
	_ = old.Close()
	for {
		if h.isClosed() {
			return nil
		}
		sctx, cancel := context.WithTimeout(ctx, h.ping)
		ps, err := h.subscribe(sctx)
		cancel()
		if err == nil {
			h.mu.Lock()
			if h.closed {
				h.mu.Unlock()
				_ = ps.Close()
				return nil
			}
			h.ps = ps
			h.mu.Unlock()
			h.resubscribed.Inc()
			h.wakeAll()
			return ps
		}
		time.Sleep(wakeRetry)
	}
}

func (h *wakeHub) isClosed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closed
}

// deliver passes a release to the waiter whose ticket it names, or to all of
// the resource's waiters when it names none. Another ticket's turn is dropped
// here, never this one's (#1809).
func (h *wakeHub) deliver(resource, ticket string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for w := range h.waiters[resource] {
		if ticket != "" && w.ticket != ticket {
			continue
		}
		signal(w.out)
	}
}

func (h *wakeHub) wakeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ws := range h.waiters {
		for w := range ws {
			signal(w.out)
		}
	}
}

// one pending signal says what a second would
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (h *wakeHub) wakes(ctx context.Context, resource, ticket string) (<-chan struct{}, func(), error) {
	if err := h.start(ctx); err != nil {
		return nil, nil, err
	}
	w := &wakeWaiter{ticket: ticket, out: make(chan struct{}, 1)}
	h.mu.Lock()
	if h.waiters[resource] == nil {
		h.waiters[resource] = make(map[*wakeWaiter]struct{})
	}
	h.waiters[resource][w] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return w.out, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			delete(h.waiters[resource], w)
			if len(h.waiters[resource]) == 0 {
				delete(h.waiters, resource)
			}
			close(w.out)
		})
	}, nil
}

func (h *wakeHub) close() {
	h.mu.Lock()
	h.closed = true
	ps := h.ps
	h.mu.Unlock()
	if ps != nil {
		_ = ps.Close()
	}
}

// globEscape keeps a configured prefix literal inside a PSUBSCRIBE pattern.
func globEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '*', '?', '[', ']', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
