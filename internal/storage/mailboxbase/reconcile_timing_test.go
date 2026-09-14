package mailboxbase_test

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
)

// reconcileWalks is how many walks the histogram has timed.
func reconcileWalks(t *testing.T) uint64 {
	t.Helper()
	m := &dto.Metric{}
	if err := mailboxbase.MetricReconcileSeconds.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("read the histogram: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

// The counter says how often the pass ran; without the histogram nothing says
// how long, which is what a throughput fix has to move (#1799).
func TestOneWalkIsTimed(t *testing.T) {
	box, inbox := gateSetup(t)
	was := reconcileWalks(t)
	deliverOutOfBand(t, inbox, "1700000001.M1P1_1.host,S=20,W=20:2,", time.Now().Add(-time.Hour))
	if _, err := box.Folder("INBOX", 1); err != nil {
		t.Fatal(err)
	}
	if now := reconcileWalks(t); now != was+1 {
		t.Errorf("the histogram counted %v walks, want one more than %v", now, was)
	}
}
