package sieve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/yarilomail/yarilo/v2/pkg/dict"
	"github.com/yarilomail/yarilo/v2/pkg/locks"
)

// sendRateFile is the home-dir file of the send counter with the file driver.
const sendRateFile = ".yarilo.sieve-send-rate"

// sendRateTTL keeps an hour's key past its hour, so pods whose clocks differ
// do not see it vanish inside the hour they count.
const sendRateTTL = 2 * time.Hour

var (
	metricSendRefused = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "sieve_send_refused_total",
		Help: "Sieve sends refused by sieve_send_rate_limit, one per recipient, by action: redirect, vacation, notify.",
	}, []string{"action"})
	metricSendLimitUnavailable = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "sieve_send_limit_unavailable_total",
		Help: "Sieve sends let through because the sieve_send_rate_limit counter could not be read, by action.",
	}, []string{"action"})
)

// sendLimit counts a mailbox's Sieve sends in a clock hour where the duplicate
// test keeps its state: a home-dir file under a lock, or the sieve_duplicate
// dict's Counter. Increment first, then compare: a send whose count is over
// the limit is refused, so concurrent pods never pass the same slot.
type sendLimit struct {
	limit   int
	driver  string
	dict    dict.Dict
	locker  locks.Locker
	now     func() time.Time
	counted func() // test seam: called on every count taken from a store
}

func newSendLimit(limit int, driver string, d dict.Dict, locker locks.Locker) *sendLimit {
	return &sendLimit{limit: limit, driver: driver, dict: d, locker: locker, now: time.Now}
}

// allow takes one recipient of action from the user's hour. Off (0) it counts
// nothing and touches no store. A store that cannot count lets the send
// through, with a warning and a metric: delivery does not stop for it.
func (l *sendLimit) allow(ctx context.Context, user, home, action string) bool {
	if l == nil || l.limit <= 0 {
		return true
	}
	n, err := l.incr(ctx, user, home)
	if err != nil {
		metricSendLimitUnavailable.WithLabelValues(action).Inc()
		slog.Warn("sieve: send counter unavailable, sending", "user", user, "action", action, "err", err)
		return true
	}
	if n > int64(l.limit) {
		metricSendRefused.WithLabelValues(action).Inc()
		slog.Warn("sieve: send over sieve_send_rate_limit, not sent", "user", user, "action", action,
			"limit", l.limit, "count", n)
		return false
	}
	return true
}

func (l *sendLimit) hour() string { return l.now().UTC().Format("2006010215") }

func (l *sendLimit) incr(ctx context.Context, user, home string) (int64, error) {
	if l.counted != nil {
		l.counted()
	}
	switch l.driver {
	case "redis":
		c, ok := l.dict.(dict.Counter)
		if !ok {
			return 0, fmt.Errorf("%w: the sieve_duplicate dict", dict.ErrNoCounter)
		}
		key := dict.PathPrivate + dict.Escape(user) + "/sieve/send-rate/" + l.hour()
		return c.Incr(ctx, &dict.OpSettings{Username: user}, key, 1, sendRateTTL)
	case "", "file":
		if home == "" {
			return 0, errors.New("no home directory for the send counter")
		}
		return l.incrFile(ctx, user, filepath.Join(home, sendRateFile))
	}
	return 0, fmt.Errorf("sieve_duplicate_driver %q keeps no send counter", l.driver)
}

// incrFile is "<hour> <count>" in one line, rewritten under the per-file lock.
func (l *sendLimit) incrFile(ctx context.Context, user, path string) (int64, error) {
	var n int64
	err := withSieveLock(ctx, l.locker, user, "sieve-send-rate:"+path, func(context.Context) error {
		hour := l.hour()
		n = 1
		if data, rerr := os.ReadFile(path); rerr == nil {
			if f := bytes.Fields(data); len(f) == 2 && string(f[0]) == hour {
				if have, perr := strconv.ParseInt(string(f[1]), 10, 64); perr == nil {
					n = have + 1
				}
			}
		} else if !errors.Is(rerr, os.ErrNotExist) {
			return rerr
		}
		tmp := path + ".tmp"
		if werr := os.WriteFile(tmp, []byte(hour+" "+strconv.FormatInt(n, 10)+"\n"), 0o600); werr != nil {
			return werr
		}
		return os.Rename(tmp, path)
	})
	return n, err
}
