package maildir

import (
	"sync"
	"time"
)

// The timestamp a temp name is minted from, shared by every backend in the
// process. Two backends on one volume mint from the same pid and host, so the
// microsecond is the only field left to separate them (#1953).
var nameTime struct {
	sync.Mutex
	secs  int64
	usecs int64
}

// mintNameTime returns a second/microsecond pair no earlier call has returned.
// When the clock has not advanced past the last name, the microsecond is
// stepped instead of waiting for it.
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
