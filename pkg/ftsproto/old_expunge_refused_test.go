package ftsproto

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The form without a message GUID is refused by name and counted: a retraction
// that quietly did nothing leaves the index answering deleted mail (#1986).
func TestTheOldExpungeFormIsRefusedAndCounted(t *testing.T) {
	before := testutil.ToFloat64(metricExpungeRefused)
	svc := &stubService{}

	got := dispatch("EXPUNGE\tu@x\tINBOX\tg1\t1\t5", svc)
	if !strings.HasPrefix(got, replyNO) {
		t.Fatalf("the old form answered %q, want a refusal", got)
	}
	if !strings.Contains(got, "guid") {
		t.Errorf("the refusal reads %q, which does not name the reason", got)
	}
	if svc.lastCmd == "expunge" {
		t.Error("the service was asked to carry out a retraction that names no message")
	}
	if now := testutil.ToFloat64(metricExpungeRefused); now != before+1 {
		t.Errorf("the refusal moved the counter by %v, want 1", now-before)
	}
}
