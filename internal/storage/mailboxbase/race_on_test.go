//go:build race

package mailboxbase_test

// raceDetector says the race detector is on: it allocates on its own, so the
// walk's cost per file is measured by the plain run of the gate.
const raceDetector = true
