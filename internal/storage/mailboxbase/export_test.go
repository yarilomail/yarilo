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

// SetBeforeHeldRecheck runs fn before the hold is taken for the second ask
// about a vanished body, and returns the undo.
func SetBeforeHeldRecheck(fn func()) func() {
	testBeforeHeldRecheck = fn
	return func() { testBeforeHeldRecheck = nil }
}

// SetAfterBodyRemoved runs fn in a body-first expunge between the body's
// removal and the record's, under the hold, and returns the undo.
func SetAfterBodyRemoved(fn func()) func() {
	testAfterBodyRemoved = fn
	return func() { testAfterBodyRemoved = nil }
}
