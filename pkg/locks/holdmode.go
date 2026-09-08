package locks

import (
	"errors"
	"fmt"
)

// HoldMode is how a hold was taken.
type HoldMode string

// The modes a goroutine can hold a resource in. HoldNone means it holds none.
const (
	HoldNone      HoldMode = ""
	HoldShared    HoldMode = "shared"
	HoldExclusive HoldMode = "exclusive"
)

// ErrHoldNotExclusive is returned when an exclusive lock is wanted on a
// resource this goroutine holds shared; acquiring would wait on itself (#1741).
var ErrHoldNotExclusive = errors.New("locks: shared hold does not satisfy an exclusive request")

// Reentrant reports the mode this goroutine holds resource in; HoldNone means
// acquire. No upgrade exists, so exclusive under a shared hold is refused.
func Reentrant(l Locker, resource, site string, shared bool) (HoldMode, error) {
	mode, held := l.HoldsResource(resource)
	switch {
	case !held:
		return HoldNone, nil
	case shared || mode == HoldExclusive:
		return mode, nil
	}
	return HoldNone, fmt.Errorf("locks: site %q wants %s exclusively while holding it %s: %w",
		site, resource, mode, ErrHoldNotExclusive)
}
