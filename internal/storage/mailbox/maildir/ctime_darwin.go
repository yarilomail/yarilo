//go:build darwin

package maildir

import "syscall"

// statCtimeNanos reads the inode-change time; the field is spelled differently
// here than on the deployment platform, and nothing else differs.
func statCtimeNanos(st *syscall.Stat_t) int64 { return st.Ctimespec.Nano() }

// statDev is the device the inode lives on; signed here, never negative.
func statDev(st *syscall.Stat_t) uint64 { return uint64(st.Dev) } //nolint:gosec // a device number
