package maildir

import (
	"math"
	"testing"
)

// The hold histogram can place a tail between 25 and 500 ms to within 25 ms:
// the acceptance of #2184 names the save section's p99 from it.
func TestTheHoldBucketsStepBy25msUpTo500ms(t *testing.T) {
	b := lockHoldBuckets()
	have := map[int]bool{}
	for i, v := range b {
		if i > 0 && v <= b[i-1] {
			t.Fatalf("bucket %d (%g) does not ascend after %g", i, v, b[i-1])
		}
		have[int(math.Round(v*1000))] = true
	}
	for ms := 25; ms <= 500; ms += 25 {
		if !have[ms] {
			t.Errorf("no bucket bound at %d ms", ms)
		}
	}
	if b[len(b)-1] < 10 {
		t.Errorf("the top bound %g s is under the 10 s list wait: a hold near the wait would read +Inf", b[len(b)-1])
	}
}
