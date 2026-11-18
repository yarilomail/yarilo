package maildir

import (
	"bytes"
	"flag"
	"fmt"
	"runtime"
	"strconv"
)

// noteListHold refuses, under test, a take of a folder's list by a goroutine
// that already holds it: the lock is a file, and a nested take would wait out
// uidListLockWait on itself. Off in production, where a second goroutine of
// the same handle waiting its turn is not told apart from a nested take.
func (u *userMailbox) noteListHold(folder, site string) func() {
	if flag.Lookup("test.v") == nil {
		return func() {}
	}
	g := goroutineID()
	if held, ok := u.listHolds.Load(folder); ok {
		if id, _ := held.(uint64); id == g {
			panic(fmt.Sprintf("maildir: %s takes the list of %q, which this goroutine already holds", site, folder))
		}
	}
	u.listHolds.Store(folder, g)
	return func() { u.listHolds.CompareAndDelete(folder, g) }
}

func goroutineID() uint64 {
	buf := make([]byte, 64)
	buf = buf[:runtime.Stack(buf, false)]
	buf = bytes.TrimPrefix(buf, []byte("goroutine "))
	if i := bytes.IndexByte(buf, ' '); i > 0 {
		buf = buf[:i]
	}
	n, _ := strconv.ParseUint(string(buf), 10, 64)
	return n
}
