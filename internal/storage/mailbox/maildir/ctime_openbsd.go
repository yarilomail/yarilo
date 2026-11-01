//go:build openbsd

package maildir

import "syscall"

// statCtimeNanos reads the inode-change time.
func statCtimeNanos(st *syscall.Stat_t) int64 { return st.Ctim.Nano() }

// statDev is the device the inode lives on; signed here, never negative.
func statDev(st *syscall.Stat_t) uint64 { return uint64(st.Dev) } //nolint:gosec // a device number
