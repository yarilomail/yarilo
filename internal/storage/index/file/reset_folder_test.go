package file

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// TestResetFolderPreservesModSeq: a rebuild that drops one record keeps every
// surviving record's own modseq (no QRESYNC modseq storm) and returns the dropped
// UID so the caller can invalidate its FTS document.
func TestResetFolderPreservesModSeq(t *testing.T) {
	b := openIdx(t.TempDir(), testUser)
	defer b.Close() //nolint:errcheck
	f, _ := b.OpenFolder("INBOX", 1, "")

	for i := uint32(1); i <= 3; i++ {
		modseq, _ := b.NextModSeq(f.ID)
		m := &mailbox.MessageMeta{UID: i, ModSeq: modseq, GUID: [16]byte{byte(i)}}
		if err := b.AppendMessage(f.ID, m); err != nil {
			t.Fatalf("append uid=%d: %v", i, err)
		}
	}

	before, _ := b.GetMessages(f.ID, mailbox.SeqSet{})
	wantModSeq := make(map[uint32]uint64, len(before))
	for _, m := range before {
		wantModSeq[m.UID] = m.ModSeq
	}

	// Keep uid 1 and 3 with their own modseqs; drop uid 2.
	keep := []*mailbox.MessageMeta{}
	for _, m := range before {
		if m.UID == 2 {
			continue
		}
		keep = append(keep, m)
	}
	expunged, err := b.ResetFolder(f.ID, keep)
	if err != nil {
		t.Fatalf("ResetFolder: %v", err)
	}
	// The identity travels with the uid: a search index retracts by the
	// message, and a dropped record names one (#1986).
	if len(expunged) != 1 || expunged[0].UID != 2 {
		t.Fatalf("expunged = %v, want uid 2", expunged)
	}
	if expunged[0].GUID == ([16]byte{}) {
		t.Error("the dropped record names no message, so nothing can retract its document")
	}

	after, _ := b.GetMessages(f.ID, mailbox.SeqSet{})
	if len(after) != 2 {
		t.Fatalf("after reset got %d messages, want 2", len(after))
	}
	for _, m := range after {
		if m.ModSeq != wantModSeq[m.UID] {
			t.Errorf("uid %d modseq = %d after reset, want preserved %d", m.UID, m.ModSeq, wantModSeq[m.UID])
		}
	}
}

// TestResetFolderStampsMissingModSeq: a record with no modseq (a freshly assigned
// UID from RebuildFolder) is stamped a fresh non-zero value bumped off the header,
// while a record that carries a modseq keeps it.
func TestResetFolderStampsMissingModSeq(t *testing.T) {
	b := openIdx(t.TempDir(), testUser)
	defer b.Close() //nolint:errcheck
	f, _ := b.OpenFolder("INBOX", 1, "")

	modseq, _ := b.NextModSeq(f.ID)
	kept := &mailbox.MessageMeta{UID: 1, ModSeq: modseq}
	if err := b.AppendMessage(f.ID, kept); err != nil {
		t.Fatalf("append: %v", err)
	}

	// uid 2 carries ModSeq 0 → must be stamped fresh.
	records := []*mailbox.MessageMeta{
		{UID: 1, ModSeq: modseq},
		{UID: 2},
	}
	if _, err := b.ResetFolder(f.ID, records); err != nil {
		t.Fatalf("ResetFolder: %v", err)
	}

	after, _ := b.GetMessages(f.ID, mailbox.SeqSet{})
	got := make(map[uint32]uint64, len(after))
	for _, m := range after {
		got[m.UID] = m.ModSeq
	}
	if got[1] != modseq {
		t.Errorf("uid 1 modseq = %d, want preserved %d", got[1], modseq)
	}
	if got[2] == 0 {
		t.Error("uid 2 (no modseq) should have been stamped a fresh non-zero modseq")
	}
	if got[2] <= modseq {
		t.Errorf("uid 2 stamped modseq = %d, want > header highest %d", got[2], modseq)
	}
}
