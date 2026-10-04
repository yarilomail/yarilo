//go:build darwin

package maildir

import "syscall"

// statCtimeNanos reads the inode-change time; the field is spelled differently
// here than on the deployment platform, and nothing else differs.
func statCtimeNanos(st *syscall.Stat_t) int64 { return st.Ctimespec.Nano() }
