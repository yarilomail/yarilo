package mailboxbase

// HoldWholeWalkBudget takes all of the walk budget, as one walk of a folder
// heavier than it does, and returns its release.
func HoldWholeWalkBudget() func() { return walkBudget.acquire(walkBudget.limit) }

// SetBeforeExpungeCheck runs fn before each index ask about a vanished body,
// and returns the undo.
func SetBeforeExpungeCheck(fn func()) func() {
	testBeforeExpungeCheck = fn
	return func() { testBeforeExpungeCheck = nil }
}
