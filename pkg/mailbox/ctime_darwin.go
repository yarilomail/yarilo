//go:build darwin

package mailbox

import (
	"os"
	"syscall"
	"time"
)

// changeTime is the inode-change time, which Chtimes moves to now.
func changeTime(fi os.FileInfo) (time.Time, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(0, st.Ctimespec.Nano()), true
}
