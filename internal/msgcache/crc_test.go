package msgcache

import (
	"testing"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A record that does not hash to what the index recorded is not this message's
// record, and every field in it is another message's answer. The check is
// before the fields, as the reference's neighbour does it.
func TestABentRecordIsARecomputeNotAnAnswer(t *testing.T) {
	idx, f, m := compatFolder(t)
	want := &imaplib.Envelope{Subject: "Plan", MessageID: "a@x"}

	fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fc == nil {
		t.Fatal("cache unavailable")
	}
	fc.StoreEnvelope(m, want)
	fc.Close()

	m = reread(t, idx, f.ID, m.UID)
	if m.CacheCRC == 0 {
		t.Fatal("the index carries no checksum, so nothing is being checked")
	}

	// The record is intact and the index's checksum is not: the same disagreement
	// a record read for the wrong message produces.
	bent := *m
	bent.CacheCRC = m.CacheCRC ^ 0xff

	fresh := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fresh == nil {
		t.Fatal("cache unavailable")
	}
	defer fresh.Close()

	was := testutil.ToFloat64(metricCRCMismatch)
	if env := fresh.Envelope(&bent); env != nil {
		t.Errorf("a record the checksum rejects was served: %+v", env)
	}
	if now := testutil.ToFloat64(metricCRCMismatch); now != was+1 {
		t.Errorf("mismatch counter = %v, want %v", now, was+1)
	}
	if env := fresh.Envelope(m); env == nil || env.Subject != "Plan" {
		t.Error("the same record read with its own checksum was refused too")
	}
}

// An index written without the extension is one the reference wrote. It is read
// at the bounds the reference reads it at: no checksum, no refusal.
func TestARecordWithNoChecksumIsRead(t *testing.T) {
	idx, f, m := compatFolder(t)
	fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fc == nil {
		t.Fatal("cache unavailable")
	}
	fc.StoreEnvelope(m, &imaplib.Envelope{Subject: "Plan"})
	fc.Close()

	m = reread(t, idx, f.ID, m.UID)
	foreign := *m
	foreign.CacheCRC = 0 // what an index from the reference carries

	fresh := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fresh == nil {
		t.Fatal("cache unavailable")
	}
	defer fresh.Close()

	was := testutil.ToFloat64(metricCRCMismatch)
	env := fresh.Envelope(&foreign)
	if env == nil || env.Subject != "Plan" {
		t.Fatal("a record with no checksum beside it was refused; an index from the reference would be unreadable")
	}
	if now := testutil.ToFloat64(metricCRCMismatch); now != was {
		t.Errorf("the absent checksum was counted as a mismatch: %v -> %v", was, now)
	}
}

// The checksum covers the whole record, not the last field written: two fields
// for one message are one record, and a checksum over half of it rejects it.
func TestTheChecksumCoversEveryFieldOfTheRecord(t *testing.T) {
	idx, f, m := compatFolder(t)
	fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fc == nil {
		t.Fatal("cache unavailable")
	}
	fc.StoreEnvelope(m, &imaplib.Envelope{Subject: "Plan"})
	fc.StoreReferences(m, []string{"<root@x>"})
	fc.StoreSizes(m, 100, 110)
	fc.Close()

	m = reread(t, idx, f.ID, m.UID)
	fresh := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fresh == nil {
		t.Fatal("cache unavailable")
	}
	defer fresh.Close()

	was := testutil.ToFloat64(metricCRCMismatch)
	if env := fresh.Envelope(m); env == nil {
		t.Error("the envelope was refused")
	}
	if refs, cached := fresh.References(m); !cached || len(refs) != 1 {
		t.Errorf("references = %v, cached = %v", refs, cached)
	}
	size, vsize, ok := fresh.Sizes(m)
	if !ok || size != 100 || vsize != 110 {
		t.Errorf("sizes = %d/%d, ok = %v", size, vsize, ok)
	}
	if now := testutil.ToFloat64(metricCRCMismatch); now != was {
		t.Errorf("a record written in three parts failed its own checksum: %v -> %v", was, now)
	}
}

// Two messages must not share a checksum: the failure the check exists for is a
// record read for the wrong message, and identical content would hide it.
func TestTwoMessagesDoNotShareAChecksum(t *testing.T) {
	idx, f, first := compatFolder(t)
	second := &mailbox.MessageMeta{UID: 2}
	if err := idx.AppendMessage(f.ID, second); err != nil {
		t.Fatal(err)
	}
	fc := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fc == nil {
		t.Fatal("cache unavailable")
	}
	fc.StoreEnvelope(first, &imaplib.Envelope{Subject: "One"})
	fc.StoreEnvelope(second, &imaplib.Envelope{Subject: "Two"})
	fc.Close()

	a := reread(t, idx, f.ID, first.UID)
	b := reread(t, idx, f.ID, second.UID)
	if a.CacheCRC == b.CacheCRC {
		t.Errorf("two different records hash the same: %d", a.CacheCRC)
	}

	// The other message's record, reached with this message's checksum.
	crossed := *a
	crossed.CacheOffset = b.CacheOffset

	fresh := Open(idx, f.ID, Options{User: "u", Folder: f.Name})
	if fresh == nil {
		t.Fatal("cache unavailable")
	}
	defer fresh.Close()
	if env := fresh.Envelope(&crossed); env != nil {
		t.Errorf("one message's checksum accepted another's record: %+v", env)
	}
}
