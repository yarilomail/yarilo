package mailboxbase

// HoldWholeWalkBudget takes all of the walk budget, as one walk of a folder
// heavier than it does, and returns its release.
func HoldWholeWalkBudget() func() { return walkBudget.acquire(walkBudget.limit) }
