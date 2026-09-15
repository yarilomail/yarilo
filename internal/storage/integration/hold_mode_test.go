package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The counter names the mode held: a read inside a locked write is carried by
// an exclusive hold, and counting it shared invents holds nobody took (#1741).
func TestTheReentrantCounterNamesTheModeHeld(t *testing.T) {
	lk := newHoldsClient(t, holdsTestSocket(t))
	home := t.TempDir()
	user := &mailbox.UserInfo{Username: "erin@example.com", Home: home}
	idx := file.New(file.WithLocker(lk)).OpenUser(user)
	t.Cleanup(func() { _ = idx.Close() })
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}

	key := locks.MailboxKey(user.Username, "INBOX")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	outer, err := lk.Lock(locks.WithSite(ctx, "write"), key, "test/0/erin@example.com/sess1", 30*time.Second)
	if err != nil {
		t.Fatalf("outer lock: %v", err)
	}
	defer func() { _ = lk.Unlock(ctx, outer.ID) }()

	// A read does not enter the hold at all now: it reads its own view and
	// takes nothing, so there is no re-entry to name (#1809).
	beforeX := reentrantCount(t, "exclusive")
	beforeS := reentrantCount(t, "shared")
	if _, err := idx.GUIDBackfillNeeded(folder.ID); err != nil {
		t.Fatalf("read under the write hold: %v", err)
	}
	if got := reentrantCount(t, "exclusive") - beforeX; got != 0 {
		t.Errorf("the read under an exclusive hold counted %v re-entries; it takes no lock", got)
	}
	if got := reentrantCount(t, "shared") - beforeS; got != 0 {
		t.Errorf("the read counted %v shared re-entries; no shared lock was ever taken", got)
	}
}

// acquireCount is how many blocking acquisitions were made on one resource
// class, counted at the client: an attempt that blocks is counted too.
func acquireCount(t *testing.T, class string) float64 {
	t.Helper()
	return sumMetric(t, "yarilo_locks_acquire_wait_seconds", "resource", class)
}

// counterValue reads one counter, summed over every series carrying label=value.
func counterValue(t *testing.T, name, label, value string) float64 {
	t.Helper()
	return sumMetric(t, name, label, value)
}

func sumMetric(t *testing.T, name, label, value string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total float64
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() != label || l.GetValue() != value {
					continue
				}
				if h := m.GetHistogram(); h != nil {
					total += float64(h.GetSampleCount())
				}
				if c := m.GetCounter(); c != nil {
					total += c.GetValue()
				}
			}
		}
	}
	return total
}

// reentrantCount reads fileindex_lock_reentrant_total for one mode, over every
// site.
func reentrantCount(t *testing.T, mode string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total float64
	for _, f := range families {
		if f.GetName() != "fileindex_lock_reentrant_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "mode" && l.GetValue() == mode {
					total += m.GetCounter().GetValue()
				}
			}
		}
	}
	return total
}
