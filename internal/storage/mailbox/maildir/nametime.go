package maildir

import (
	"sync"
	"time"
)

// Two backends in one process share pid and host, so the microsecond is the
// only field left to separate their names (#1953).
var nameTime struct {
	sync.Mutex
	secs  int64
	usecs int64
}

// mintNameTime returns a second/microsecond pair no earlier call returned,
// stepping the microsecond when the clock has not advanced.
func mintNameTime() (int64, int64) {
	now := time.Now()
	secs, usecs := now.Unix(), int64(now.Nanosecond()/1000)

	nameTime.Lock()
	defer nameTime.Unlock()
	if secs < nameTime.secs || (secs == nameTime.secs && usecs <= nameTime.usecs) {
		secs, usecs = nameTime.secs, nameTime.usecs+1
		if usecs >= 1_000_000 {
			secs++
			usecs = 0
		}
	}
	nameTime.secs, nameTime.usecs = secs, usecs
	return secs, usecs
}
