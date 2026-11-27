package dict

import (
	"context"
	"errors"
	"time"
)

// Counter is a driver that adds to an integer key and returns the sum in one
// step, creating the key when it is missing. A transaction's AtomicInc does
// neither: it returns no value and fails on a missing key, so a limit built on
// it would need a read and a create that race between processes.
type Counter interface {
	// Incr adds delta to key and returns the new value. ttl, when above zero,
	// is the key's lifetime from this call.
	Incr(ctx context.Context, set *OpSettings, key string, delta int64, ttl time.Duration) (int64, error)
}

// ErrNoCounter is a dict that cannot count: its driver has no Counter, or the
// dict service it goes through predates the operation.
var ErrNoCounter = errors.New("dict: no counter")
