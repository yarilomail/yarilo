//go:build freebsd || linux

package master

import "syscall"

// setDeathSignal asks the kernel to send SIGTERM to the component when the
// supervisor dies. On FreeBSD it is procctl(PROC_PDEATHSIG_CTL), set by the Go
// runtime between fork and exec, and it is per process.
func setDeathSignal(a *syscall.SysProcAttr) { a.Pdeathsig = syscall.SIGTERM }
