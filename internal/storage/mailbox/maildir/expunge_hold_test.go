package maildir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

const holdBody = "From: a@b\r\nSubject: x\r\n\r\nbody\r\n"

// fillFolder saves n messages into INBOX at uids 1..n.
func fillFolder(t *testing.T, box *userMailbox, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		name, _, _, err := box.Save("INBOX", strings.NewReader(holdBody), 0, int64(len(holdBody)), nil, nil, [16]byte{})
		if err != nil {
			t.Fatal(err)
		}
		if _, aerr := box.AssignUID("INBOX", name, uint32(i)); aerr != nil {
			t.Fatal(aerr)
		}
	}
}

// An expunge names each message and removes it, all under one hold. Reading the
// directory once per removal is what put 737ms of walk inside that hold (#1809).
func TestExpungingManyReadsTheDirectoryOnce(t *testing.T) {
	box, _, _ := recSetup(t)
	const n = 10
	fillFolder(t, box, n)

	ResetDirReads()
	for i := 1; i <= n; i++ {
		name, err := mailboxbase.MessagePath(box, "INBOX", &mailbox.MessageMeta{UID: uint32(i)})
		if err != nil {
			t.Fatalf("uid %d has no name on disk: %v", i, err)
		}
		if rerr := box.RemoveHeld("INBOX", name); rerr != nil {
			t.Fatalf("uid %d: %v", i, rerr)
		}
	}
	if got := DirReads(); got != 1 {
		t.Errorf("removing %d messages read the directory %d times, want 1", n, got)
	}
}

// The cheaper path must not let the listing keep naming a file this process
// removed: the name it hands back next is the one the removal would take.
func TestARemovedNameIsGoneFromTheListing(t *testing.T) {
	box, _, _ := recSetup(t)
	fillFolder(t, box, 3)

	name, err := mailboxbase.MessagePath(box, "INBOX", &mailbox.MessageMeta{UID: 2})
	if err != nil {
		t.Fatal(err)
	}
	if rerr := box.Remove("INBOX", name); rerr != nil {
		t.Fatal(rerr)
	}
	if _, err := mailboxbase.MessagePath(box, "INBOX", &mailbox.MessageMeta{UID: 2}); err == nil {
		t.Error("the listing still names the message that was just removed")
	}

	left, lerr := box.Scan("INBOX")
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(left) != 2 {
		t.Errorf("the folder holds %d messages after one removal, want 2", len(left))
	}
	for _, m := range left {
		if m.Filename == name {
			t.Errorf("a walk still finds %q", name)
		}
	}
}

// holdSamples is how many holds the histogram has recorded for one site.
func holdSamples(t *testing.T, site string) uint64 {
	t.Helper()
	obs, err := metricLockHold.GetMetricWithLabelValues(site)
	if err != nil {
		t.Fatalf("hold histogram for %q: %v", site, err)
	}
	var m dto.Metric
	if werr := obs.(prometheus.Metric).Write(&m); werr != nil {
		t.Fatalf("read hold histogram for %q: %v", site, werr)
	}
	return m.GetHistogram().GetSampleCount()
}

// How long a site holds the lock is its cost to every other session on that
// folder, and a count of acquisitions does not carry it (#1809).
func TestAHoldIsTimedUnderTheSiteThatTookIt(t *testing.T) {
	box, _ := batchBox(t)
	const site = "expunge"
	before := holdSamples(t, site)
	beforeSave := holdSamples(t, lockSiteSave)

	if err := box.HoldFolder("INBOX", site, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := holdSamples(t, site); got != before+1 {
		t.Errorf("one hold recorded %d samples under %q, want %d", got-before, site, 1)
	}

	// And under the site that took it, not under one name for all of them.
	if got := holdSamples(t, lockSiteSave); got != beforeSave {
		t.Errorf("a hold taken for %q was also recorded under %q", site, lockSiteSave)
	}
}

// A removal outside a hold takes the listing with it: the cheap path needs a
// holder to close the unlink-to-stat window (#1809).
func TestAnUnheldRemovalDropsTheListing(t *testing.T) {
	box, _, _ := recSetup(t)
	fillFolder(t, box, 3)

	name, err := mailboxbase.MessagePath(box, "INBOX", &mailbox.MessageMeta{UID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, primed := box.folderCacheFor("INBOX").dirEntries(dirMtime(t, box, "INBOX")); !primed {
		t.Fatal("the listing was not cached, so this row would prove nothing")
	}
	if rerr := box.Remove("INBOX", name); rerr != nil {
		t.Fatal(rerr)
	}
	if box.folderCacheFor("INBOX").entries != nil {
		t.Error("an unheld removal kept the cached listing")
	}
}

// dirMtime is the key the listing cache is stored under.
func dirMtime(t *testing.T, box *userMailbox, folder string) time.Time {
	t.Helper()
	fi, err := os.Stat(filepath.Join(box.folderPath(folder), "cur"))
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}
