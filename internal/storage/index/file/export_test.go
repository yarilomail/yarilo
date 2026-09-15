package file

// onLogRead counts the calls that read the journal.
func onLogRead(fn func()) func() {
	logRead = fn
	return func() { logRead = nil }
}
