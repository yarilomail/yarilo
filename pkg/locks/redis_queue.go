package locks

import (
	"context"
	"fmt"
	"sync"

	"github.com/redis/go-redis/v9"
)

// queueKey is the arrival order on a resource: a ZSET scored by arrival, so
// the front is ZRANGE 0 0 and an abandoned ticket ages out of it.
func (b *RedisBackend) queueKey(resource string) string {
	return b.keyPrefix + "q:" + resource
}

// enqueueScript adds a ticket, drops abandoned ones, and answers how many
// stand ahead of it.
//
//	KEYS[1] = queue key
//	ARGV[1] = ticket
//	ARGV[2] = abandon window in ms
var enqueueScript = redis.NewScript(`
local now = redis.call("TIME")
local nowMs = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
redis.call("ZREMRANGEBYSCORE", KEYS[1], "-inf", nowMs - tonumber(ARGV[2]))
redis.call("ZADD", KEYS[1], "NX", nowMs, ARGV[1])
return redis.call("ZRANK", KEYS[1], ARGV[1])
`)

// Enqueue implements WaitQueue.
func (b *RedisBackend) Enqueue(ctx context.Context, resource, ticket string) (int, error) {
	res, err := enqueueScript.Run(ctx, b.rdb,
		[]string{b.queueKey(resource)}, ticket, ticketTTL.Milliseconds()).Result()
	if err != nil {
		return 0, fmt.Errorf("locks/redis: enqueue: %w", err)
	}
	rank, _ := res.(int64)
	return int(rank), nil
}

// AtFront implements WaitQueue.
func (b *RedisBackend) AtFront(ctx context.Context, resource, ticket string) (bool, error) {
	front, err := b.rdb.ZRange(ctx, b.queueKey(resource), 0, 0).Result()
	if err != nil {
		return false, fmt.Errorf("locks/redis: front: %w", err)
	}
	// An empty line means this ticket aged out: try, rather than wait for a
	// turn nobody is left to hand over.
	if len(front) == 0 {
		return true, nil
	}
	return front[0] == ticket, nil
}

// Dequeue implements WaitQueue.
func (b *RedisBackend) Dequeue(ctx context.Context, resource, ticket string) error {
	if err := b.rdb.ZRem(ctx, b.queueKey(resource), ticket).Err(); err != nil {
		return fmt.Errorf("locks/redis: dequeue: %w", err)
	}
	return nil
}

// wakeChannel is the release channel for a resource, kept apart from the event
// channel so a mailbox notification cannot be read as a lock handover.
func (b *RedisBackend) wakeChannel(resource string) string {
	return b.chPrefix + "unlocked:" + resource
}

// Wakes implements WaitQueue. Every replica subscribes, so a release anywhere
// reaches the contender waiting here.
func (b *RedisBackend) Wakes(ctx context.Context, resource string) (<-chan struct{}, func(), error) {
	ps := b.rdb.Subscribe(ctx, b.wakeChannel(resource))
	if _, err := ps.Receive(ctx); err != nil {
		_ = ps.Close()
		return nil, nil, fmt.Errorf("locks/redis: wakes: %w", err)
	}
	out := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(out)
		msgs := ps.Channel()
		for {
			select {
			case <-done:
				return
			case _, ok := <-msgs:
				if !ok {
					return
				}
				select {
				case out <- struct{}{}:
				default: // one pending signal is enough
				}
			}
		}
	}()
	var once sync.Once
	return out, func() {
		once.Do(func() {
			close(done)
			_ = ps.Close()
		})
	}, nil
}
