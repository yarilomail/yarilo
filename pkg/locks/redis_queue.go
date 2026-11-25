package locks

import (
	"context"
	"fmt"
	"time"

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

// dequeueScript takes a ticket out of the line and hands the turn to whoever
// now stands first, so a contender that gave up does not leave it to the
// timer.
//
//	KEYS[1] = queue key
//	ARGV[1] = ticket
//	ARGV[2] = the resource's release channel
var dequeueScript = redis.NewScript(luaParseValue + `
leaveLine(KEYS[1], ARGV[1], ARGV[2])
return 1
`)

// Dequeue implements WaitQueue.
func (b *RedisBackend) Dequeue(ctx context.Context, resource, ticket string) error {
	if err := dequeueScript.Run(ctx, b.rdb, []string{b.queueKey(resource)}, ticket, b.wakeChannel(resource)).Err(); err != nil {
		return fmt.Errorf("locks/redis: dequeue: %w", err)
	}
	return nil
}

// JoinAndTry implements WaitQueue in one round trip.
func (b *RedisBackend) JoinAndTry(ctx context.Context, resource, owner, site, ticket string, ttl time.Duration, shared bool) (int, string, Holder, error) {
	if resource == "" || owner == "" {
		return 0, "", Holder{}, fmt.Errorf("locks/redis: resource and owner must be non-empty")
	}
	id, err := randID()
	if err != nil {
		return 0, "", Holder{}, fmt.Errorf("locks/redis: generate id: %w", err)
	}
	script, kind := joinAcquireScript, "x"
	if shared {
		script, kind = joinAcquireSharedScript, "s"
	}
	res, err := script.Run(ctx, b.rdb,
		[]string{b.resKey(resource), b.lockKey(id), b.sharedKey(resource), b.queueKey(resource)},
		id, lockValue(resource, owner, kind, site), ttl.Milliseconds(), b.keyPrefix, ticket,
		b.wakeChannel(resource), ticketTTL.Milliseconds(),
	).Result()
	if err != nil {
		return 0, "", Holder{}, fmt.Errorf("locks/redis: join and try: %w", err)
	}
	if arr, ok := res.([]interface{}); ok && len(arr) == 2 && arr[0] == "QUEUED" {
		rank, _ := arr[1].(int64)
		return int(rank), "", Holder{}, nil
	}
	id, h, err := parseAcquireResult(res, id)
	return 0, id, h, err
}

// wakeChannel is the release channel for a resource, kept apart from the event
// channel so a mailbox notification cannot be read as a lock handover.
func (b *RedisBackend) wakeChannel(resource string) string {
	return b.chPrefix + "unlocked:" + resource
}

// Wakes implements WaitQueue. Every replica listens on its one shared
// subscription, so a release anywhere reaches the contender waiting here.
func (b *RedisBackend) Wakes(ctx context.Context, resource, ticket string) (<-chan struct{}, func(), error) {
	return b.wakes.wakes(ctx, resource, ticket)
}
