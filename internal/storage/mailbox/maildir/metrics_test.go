package maildir

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Each acquisition is counted under the call that took it, not under one name
// for all of them.
//
// The point of the label is to tell this driver's share of a key it shares with
// the index apart from the index's own -- and to tell a delivery from a poll
// inside that share (#1630). A single total answers neither question, so the
// test asserts the split rather than the sum.
func TestLockAcquisitionsAreCountedByCaller(t *testing.T) {
	box, _ := batchBox(t)
	body := "From: a@b\r\n\r\nx\r\n"

	before := map[string]float64{}
	for _, site := range []string{lockSiteSave, lockSiteWriteFlagsBulk, lockSiteCreate} {
		before[site] = testutil.ToFloat64(metricLockAcquired.WithLabelValues(site))
	}

	// The save itself: one acquisition. The list entry is written inside the
	// caller's own hold of the same key, which the next row measures (#1700).
	saved, _, _, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	_ = saved
	if err := box.Create("Work"); err != nil {
		t.Fatal(err)
	}
	msgs, lerr := box.List("INBOX")
	if lerr != nil || len(msgs) != 1 {
		t.Fatalf("list = %v, err = %v", msgs, lerr)
	}
	// The flag write takes the name off disk, which is where it lives.
	scanned, serr := box.Scan("INBOX")
	if serr != nil || len(scanned) != 1 {
		t.Fatalf("scan = %v, err = %v", scanned, serr)
	}
	box.WriteFlagsMulti("INBOX", []mailbox.FlagWrite{
		{UID: 1, Filename: scanned[0].Filename, Flags: []string{`\Seen`}},
	})

	for site, want := range map[string]float64{
		lockSiteSave:           1,
		lockSiteCreate:         1,
		lockSiteWriteFlagsBulk: 1,
	} {
		got := testutil.ToFloat64(metricLockAcquired.WithLabelValues(site)) - before[site]
		if got != want {
			t.Errorf("site %q counted %v acquisitions, want %v", site, got, want)
		}
	}
	// And nothing landed under a site that did no work.
	if got := testutil.ToFloat64(metricLockAcquired.WithLabelValues(lockSiteMove)); got != 0 {
		t.Errorf("site %q counted %v acquisitions with no move done", lockSiteMove, got)
	}
}

// Naming inside a hold of the folder key costs no acquisition of its own; a
// delivery, which holds none, pays one (#1700, #1706).
func TestNamingCostsNothingInsideACycleAndOneHoldOutside(t *testing.T) {
	box, l := batchBox(t)
	body := "From: a@b\r\n\r\nx\r\n"

	saved, _, _, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	before := l.acquires.Load()
	// Inside a hold of the same key, as AllocateAndAppendNamed runs it.
	if err := func() error {
		ctx := context.Background()
		lk, aerr := l.Lock(ctx, locks.MailboxKey("u@x.com", "INBOX"), "owner", time.Minute)
		if aerr != nil {
			return aerr
		}
		defer l.Unlock(ctx, lk.ID) //nolint:errcheck
		_, nerr := box.AssignUID("INBOX", saved, 1)
		return nerr
	}(); err != nil {
		t.Fatal(err)
	}
	inside := l.acquires.Load() - before

	saved2, _, _, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	before = l.acquires.Load()
	if _, err := box.AssignUID("INBOX", saved2, 2); err != nil {
		t.Fatal(err)
	}
	outside := l.acquires.Load() - before

	if inside != 1 {
		t.Errorf("naming inside a hold took %d acquisitions, want 1 (the hold itself)", inside)
	}
	if outside != 1 {
		t.Errorf("naming outside a hold took %d acquisitions, want 1", outside)
	}
}
