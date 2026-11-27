//go:build !freebsd && !linux

package master

import "syscall"

// setDeathSignal has no kernel support here; the lock, the recorded pids and
// the log pipe still keep a component from outliving the supervisor.
func setDeathSignal(*syscall.SysProcAttr) {}
