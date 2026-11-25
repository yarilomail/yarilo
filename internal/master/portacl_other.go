//go:build !freebsd

package master

// HostPreStart has nothing to check off FreeBSD.
func HostPreStart(Component) error { return nil }
