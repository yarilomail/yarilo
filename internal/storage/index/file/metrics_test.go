package file

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func histSum(t *testing.T, h prometheus.Histogram) (float64, uint64) {
	t.Helper()
	var m dto.Metric
	if err := h.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	return m.GetHistogram().GetSampleSum(), m.GetHistogram().GetSampleCount()
}

func histVecSum(t *testing.T, v *prometheus.HistogramVec, label string) (float64, uint64) {
	t.Helper()
	h, err := v.GetMetricWithLabelValues(label)
	if err != nil {
		t.Fatalf("get %s: %v", label, err)
	}
	var m dto.Metric
	if err := h.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write %s: %v", label, err)
	}
	return m.GetHistogram().GetSampleSum(), m.GetHistogram().GetSampleCount()
}

// The parts must reconcile with the whole, or an analysis is left with a
// nameless remainder and no way to tell a fourth cost from a measurement bug.
// The named parts are the lock, the freshness check and building the answer;
// what the total holds beyond them is the finding, and it cannot be negative.
func TestReadPartsFitInsideTheWhole(t *testing.T) {
	root := t.TempDir()
	ui := openIdx(root, "alice@example.com")
	f, err := ui.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Size: 10}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	// The fallback: the only read that still has a lock part to time (#1809).
	fsParts := ui.open[f.ID]
	fsParts.mu.Lock()
	fsParts.lineage = lineageHdr{}
	fsParts.mu.Unlock()

	whole, wholeCount := histSum(t, metricReadSeconds)
	lock, lockCount := histVecSum(t, metricReadPart, "lock")
	reload, reloadCount := histVecSum(t, metricReadPart, "reload")
	build, buildCount := histVecSum(t, metricReadPart, "build")

	if _, err := ui.GetMessages(f.ID, mailbox.SeqSet{}); err != nil {
		t.Fatalf("GetMessages: %v", err)
	}

	wholeAfter, wholeCountAfter := histSum(t, metricReadSeconds)
	lockAfter, lockCountAfter := histVecSum(t, metricReadPart, "lock")
	reloadAfter, reloadCountAfter := histVecSum(t, metricReadPart, "reload")
	buildAfter, buildCountAfter := histVecSum(t, metricReadPart, "build")

	if wholeCountAfter == wholeCount {
		t.Fatal("the read was not timed at all")
	}
	for name, moved := range map[string]bool{
		"lock":   lockCountAfter > lockCount,
		"reload": reloadCountAfter > reloadCount,
		"build":  buildCountAfter > buildCount,
	} {
		if !moved {
			t.Errorf("a read did not time its %s part", name)
		}
	}
	parts := (lockAfter - lock) + (reloadAfter - reload) + (buildAfter - build)
	if total := wholeAfter - whole; parts > total {
		t.Errorf("parts sum to %.6fs inside a whole of %.6fs: the spans overlap", parts, total)
	}
}

// Every read leaves the process to take a shared lock, which is the one thing
// the reference does not do here — it takes a local fcntl. The count is what
// turns "the lock is suspected" into a number, so a read must be counted as a
// round trip and not, say, as a re-entrant hit.
func TestReadsCountTheirLockRoundTrips(t *testing.T) {
	root := t.TempDir()
	ui := openIdx(root, "bob@example.com")
	f, err := ui.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}

	// No lock service is wired in unit tests, so the counters must stay still:
	// counting a round trip that did not happen would put an operator's eye on
	// a cost their deployment does not pay.
	before := sharedAcquisitions(t)
	if _, err := ui.GetMessages(f.ID, mailbox.SeqSet{}); err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if got := sharedAcquisitions(t); got != before {
		t.Errorf("a read counted %v lock acquisitions with no lock service wired", got-before)
	}
}

// sharedAcquisitions totals shared-mode acquisitions across every site, which
// is what a caller cares about when asking "did this read leave the process".
func sharedAcquisitions(t *testing.T) float64 {
	t.Helper()
	var total float64
	// Shared acquisitions only: no write site takes a shared lock.
	for _, site := range []string{lockSiteOpenProbe, lockSiteFallback} {
		total += counterVecValue(t, metricLockAcquired, "shared", site)
	}
	return total
}

func counterVecValue(t *testing.T, v *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	c, err := v.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("get counter %v: %v", labels, err)
	}
	var m dto.Metric
	if err := c.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write counter %v: %v", labels, err)
	}
	return m.GetCounter().GetValue()
}

// Every reader that only answers a client must take zero round trips, and every
// reader whose answer decides a write must still take them. Enumerated rather
// than sampled: the classification in #1249 is the deliverable, and a table is
// how it stays checkable when someone adds a reader.
func TestReadersTakeTheLockTheirClassificationSays(t *testing.T) {
	dial := raceTestLockServer(t)
	root := t.TempDir()
	home := testHome(root, "frank@example.com")
	ui := New(WithLocker(dial())).OpenUser(&mailbox.UserInfo{
		Username: "frank@example.com", Home: home,
	}).(*userHandle).ui

	f, err := ui.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Size: 10}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	tests := []struct {
		name string
		read func() error
	}{
		{"messages", func() error { _, e := ui.GetMessages(f.ID, mailbox.SeqSet{}); return e }},
		{"vanished", func() error { _, e := ui.Vanished(f.ID, 0); return e }},
		{"keywords", func() error { _, e := ui.Keywords(f.ID); return e }},
		{"sizeless uids", func() error { _, e := ui.SizelessUIDs(f.ID); return e }},
		{"pop3 uidls", func() error { _, e := ui.pop3UIDLs(f.ID); return e }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := sharedAcquisitions(t)
			done := make(chan error, 1)
			// Own goroutine: holds are tracked per goroutine, so a read issued
			// from the writer's would take the re-entrant path and measure
			// nothing while passing.
			go func() { done <- tc.read() }()
			if err := <-done; err != nil {
				t.Fatalf("read: %v", err)
			}
			if got := sharedAcquisitions(t) - before; got != 0 {
				t.Errorf("%v round trips taken by a read; a reader takes no lock (#1809)", got)
			}
		})
	}
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("write counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

// A write cycle holds the journal once, however many records it appends: the
// kernel lock is not re-entrant, so a second take would deadlock (#1840).
func TestAWriteCycleHoldsTheJournalOnce(t *testing.T) {
	root := t.TempDir()
	ui := openIdx(root, "gina@example.com")
	f, err := ui.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	before := exclusiveAcquisitions()
	if err := ui.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Size: 10}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if got := exclusiveAcquisitions() - before; got != 1 {
		t.Errorf("one append held the journal %v times, want 1", got)
	}

	// And a read holds nothing at all.
	before = exclusiveAcquisitions()
	if _, err := ui.GetMessages(f.ID, mailbox.SeqSet{}); err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if got := exclusiveAcquisitions() - before; got != 0 {
		t.Errorf("a read held the journal %v times, want 0", got)
	}
}
