package file

import (
	"testing"
	"time"
)

// The fold age is a decision with a number behind it, so the number is pinned
// rather than left to whoever edits the const block next.
//
// 60s came out of the #1460 window: at 300s an actively delivering account
// carried its log to ~110 KB past the floor and paid p99 249ms against 128ms.
// Changing it back would be a product decision, and this row is where that
// decision has to be made deliberately instead of by a keystroke.
func TestTheFoldAgeDefaultIsTheMeasuredOne(t *testing.T) {
	const measured = 60 * time.Second
	if got := New().logCompactMinAge; got != measured {
		t.Errorf("built-in fold age = %s, want %s -- see #1460 before changing it", got, measured)
	}
}

// Each arm is taken as given, a 0 included: 0 turns that arm off (#1481).
func TestEachRotationArmIsTakenAsGiven(t *testing.T) {
	tests := []struct {
		name              string
		minBytes, maxByte int64
		minAge            time.Duration
	}{
		{name: "all three off"},
		{name: "the age alone", minAge: 60 * time.Second},
		{name: "the ceiling alone", maxByte: 64 * 1024},
		{name: "the floor alone", minBytes: 8 * 1024},
		{name: "all three", minBytes: 8 * 1024, maxByte: 64 * 1024, minAge: 30 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := New(WithLogCompaction(tc.minBytes, tc.maxByte, tc.minAge))
			if b.logCompactMinBytes != tc.minBytes || b.logCompactMaxBytes != tc.maxByte || b.logCompactMinAge != tc.minAge {
				t.Errorf("triple = %d/%d/%s, want %d/%d/%s", b.logCompactMinBytes, b.logCompactMaxBytes, b.logCompactMinAge,
					tc.minBytes, tc.maxByte, tc.minAge)
			}
		})
	}
}
