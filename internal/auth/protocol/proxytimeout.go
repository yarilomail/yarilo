package protocol

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ParseProxyTimeout reads a proxy_timeout value: a bare number is seconds,
// anything else a number with a time unit. "" and "0" mean the global value.
func ParseProxyTimeout(v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	if n, err := strconv.ParseUint(v, 10, 32); err == nil {
		return checkProxyTimeout(v, n*1000)
	}
	i := 0
	for i < len(v) && v[i] >= '0' && v[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("invalid time interval: %q", v)
	}
	n, err := strconv.ParseUint(v[:i], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("time interval is too large: %q", v)
	}
	unit := strings.ToLower(strings.TrimLeft(v[i:], " "))
	var ms uint64
	switch {
	case unit == "":
		return 0, fmt.Errorf("time interval %q is missing units", v)
	case isPrefixOf(unit, "secs", "seconds"):
		ms = 1000
	case isPrefixOf(unit, "mins", "minutes"): // before msecs: a bare "m" is minutes
		ms = 60 * 1000
	case isPrefixOf(unit, "msecs", "mseconds", "millisecs", "milliseconds"):
		ms = 1
	case isPrefixOf(unit, "hours"):
		ms = 60 * 60 * 1000
	case isPrefixOf(unit, "days"):
		ms = 24 * 60 * 60 * 1000
	case isPrefixOf(unit, "weeks"):
		ms = 7 * 24 * 60 * 60 * 1000
	default:
		return 0, fmt.Errorf("invalid time interval: %q", v)
	}
	if n > math.MaxUint32/ms {
		return 0, fmt.Errorf("time interval is too large: %q", v)
	}
	return checkProxyTimeout(v, n*ms)
}

func checkProxyTimeout(v string, ms uint64) (time.Duration, error) {
	if ms > math.MaxUint32 {
		return 0, fmt.Errorf("time interval is too large: %q", v)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

func isPrefixOf(unit string, words ...string) bool {
	for _, w := range words {
		if strings.HasPrefix(w, unit) {
			return true
		}
	}
	return false
}
