//go:build linux

package maildir

import "syscall"

// statCtimeNanos reads the inode-change time. A rename-over always moves it,
// which is what tells two files apart when inode, size and mtime agree.
func statCtimeNanos(st *syscall.Stat_t) int64 { return st.Ctim.Nano() }
