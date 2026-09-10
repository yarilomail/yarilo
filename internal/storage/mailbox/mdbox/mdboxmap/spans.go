package mdboxmap

// The two spans of a locked operation: the wait, then the work under it. What
// keeps them apart is where each begins, not how long either takes.
const (
	spanWaitStart = "wait-start"
	spanWaitEnd   = "wait-end"
	spanHoldStart = "hold-start"
	spanHoldEnd   = "hold-end"
)

// spanEvents receives those boundaries when a test asks for them. Test seam: a
// row that separates the spans by duration is a row a busy machine fails (#1738).
var spanEvents func(string)

// SetTestSpanRecorder installs the recorder and returns a function removing it.
func SetTestSpanRecorder(fn func(string)) func() {
	spanEvents = fn
	return func() { spanEvents = nil }
}

func noteSpan(name string) {
	if spanEvents != nil {
		spanEvents(name)
	}
}
