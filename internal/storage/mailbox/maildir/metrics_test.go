package maildir

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Only a call that writes a shared file takes a hold: a create, a flag write
// and a move each rename or mkdir, which excludes nobody (#1840).
func TestLockAcquisitionsAreCountedByCaller(t *testing.T) {
	box := batchBox(t)
	body := "From: a@b\r\n\r\nx\r\n"

	before := map[string]float64{}
	for _, site := range []string{lockSiteSave, lockSiteWriteFlagsBulk, lockSiteCreate, lockSiteMove} {
		before[site] = testutil.ToFloat64(metricLockAcquired.WithLabelValues(site))
	}

	// The save itself: one acquisition. The list entry is written inside the
	// caller's own hold of the same key, which the next row measures (#1700).
	saved, _, _, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if _, aerr := box.AssignUID("INBOX", saved, 1); aerr != nil {
		t.Fatalf("assign uid: %v", aerr)
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
		lockSiteCreate:         0,
		lockSiteWriteFlagsBulk: 0,
		lockSiteMove:           0,
	} {
		got := testutil.ToFloat64(metricLockAcquired.WithLabelValues(site)) - before[site]
		if got != want {
			t.Errorf("site %q counted %v acquisitions, want %v", site, got, want)
		}
	}
}
