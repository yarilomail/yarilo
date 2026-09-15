package maildir

import "time"

// SetUIDListLockWait shortens the wait a test spends proving that a held lock
// stops a write.
func SetUIDListLockWait(d time.Duration) func() {
	prev := uidListLockWait
	uidListLockWait = d
	return func() { uidListLockWait = prev }
}
