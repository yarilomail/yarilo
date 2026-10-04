package locks

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
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

// holdUpgradeRefused counts writes refused for holding the resource shared.
// Zero is the expected reading: each one is a user operation lost (#1741).
var holdUpgradeRefused = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "yarilo_locks_hold_upgrade_refused_total",
	Help: "Exclusive lock requests refused because the calling goroutine already held the resource shared, by the site that asked. Every one is a user operation that failed; the fix is at the call site that nests a write inside a read.",
}, []string{"site"})

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
	if site == "" {
		site = SiteUnknown
	}
	holdUpgradeRefused.WithLabelValues(site).Inc()
	slog.Error("locks: exclusive request refused under a shared hold",
		"site", site, "resource", resource, "held", string(mode))
	return HoldNone, fmt.Errorf("locks: site %q wants %s exclusively while holding it %s: %w",
		site, resource, mode, ErrHoldNotExclusive)
}
