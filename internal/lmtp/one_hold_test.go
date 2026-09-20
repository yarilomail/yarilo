package lmtp

import (
	"bytes"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// journalHolds sums the file locks the index has taken, from the default
// registry: the count lives in another package, the number is the same one.
func journalHolds(t *testing.T) float64 {
	t.Helper()
	fams, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, f := range fams {
		if f.GetName() != "fileindex_lock_acquired_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			total += m.GetCounter().GetValue()
		}
	}
	return total
}

// A delivery holds the journal once: it took the folder three times -- uid,
// modseq, record -- with two windows between them (#1706, #1840).
func TestADeliveryTakesTheFolderOnce(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "maildir"}
	store := maildir.New().OpenUser(info)
	idx := file.New().OpenUser(info)
	t.Cleanup(func() { _ = store.Close(); _ = idx.Close() })
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	// The door a delivery uses: it adds a message and settles nothing, so the
	// walk buys nothing and the hold it costs is the one this row counts.
	box := mailboxbase.Open(store, idx, mailboxbase.SaveOnly())
	if _, err := box.Folder("INBOX", 1); err != nil {
		t.Fatal(err)
	}

	const raw = "From: a@b\r\nSubject: one hold\r\n\r\nbody\r\n"
	before := journalHolds(t)
	scansBefore := reconcileCount("scanned")
	uid, _, _, err := deliverOne(box, "INBOX", bytes.NewReader([]byte(raw)), int64(len(raw)), nil, info.Username, "x@y", nil)
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if uid == 0 {
		t.Fatal("the delivery reported uid 0")
	}
	if got := journalHolds(t) - before; got != 1 {
		t.Errorf("the delivery held the journal %v times, want 1", got)
	}
	if n := reconcileCount("scanned") - scansBefore; n != 0 {
		t.Errorf("the delivery walked the folder %v times, want none", n)
	}

	// And the message is there, named, as any delivery must leave it.
	msgs, err := box.Messages(1, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("the folder holds %d records", len(msgs))
	}
	name, perr := box.MessagePath("INBOX", msgs[0])
	if perr != nil || name == "" {
		t.Fatalf("the delivered record names no file: %q %v", name, perr)
	}
	rc, oerr := box.OpenMessage("INBOX", msgs[0])
	if oerr != nil {
		t.Fatalf("the delivered message cannot be read: %v", oerr)
	}
	defer rc.Close() //nolint:errcheck
	body := make([]byte, len(raw))
	if _, rerr := rc.Read(body); rerr != nil && !strings.Contains(rerr.Error(), "EOF") {
		t.Fatalf("read: %v", rerr)
	}
}

// reconcileCount sums one decision over every reason: these rows count walks,
// not their causes.
func reconcileCount(result string) float64 {
	n := 0.0
	for _, reason := range []string{"", "first-seen", "token-moved", "hot-new", "owed"} {
		n += testutil.ToFloat64(mailboxbase.MetricReconcile.WithLabelValues(result, reason))
	}
	return n
}
