package file

import (
	"math/bits"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// pendingFolder is a folded folder of n records, its image dropped from the
// cache, as another process finds it.
func pendingFolder(t *testing.T, n int) string {
	t.Helper()
	home := movedFolder(t, n)
	o, of := acceptOpen(t, home)
	if err := o.OptimizeIndex(of.ID); err != nil {
		t.Fatal(err)
	}
	o.Close() //nolint:errcheck
	baseImages.Delete(indexPathFor(home))
	return home
}

// deliver opens the folder and appends one message, as LMTP does, and says
// how many times it loaded chunks of the base.
func deliverCold(t *testing.T, home string) int64 {
	t.Helper()
	baseImages.Delete(indexPathFor(home))
	opens := mailindex.LazyOpens()
	h, f := acceptOpen(t, home)
	defer h.Close() //nolint:errcheck
	uid, err := h.AllocateUID(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid, Size: 10}); err != nil {
		t.Fatal(err)
	}
	return mailindex.LazyOpens() - opens
}

func coldStatus(t *testing.T, home string) *mailbox.Folder {
	t.Helper()
	baseImages.Delete(indexPathFor(home))
	h, f := acceptOpen(t, home)
	defer h.Close() //nolint:errcheck
	return f
}

// Another session's \Seen on a chunk not loaded, then a delivery: a cold
// reader's STATUS UNSEEN is right, and the delivery loaded no chunk
// (INDEX-MEMORY.md §4, reviewer 2026-11-20).
func TestADeliveryAfterAForeignSeenReadsNoChunk(t *testing.T) {
	const n = 4 * mailindex.ChunkRecords
	home := pendingFolder(t, n)
	o, of := acceptOpen(t, home)
	if err := o.UpdateFlags(of.ID, 3*mailindex.ChunkRecords+1, nil, nil); err != nil {
		t.Fatal(err)
	}
	o.Close() //nolint:errcheck
	before := coldStatus(t, home).Unseen
	o, of = acceptOpen(t, home)
	if err := o.UpdateFlags(of.ID, 3*mailindex.ChunkRecords+1, []string{`\Seen`}, nil); err != nil {
		t.Fatal(err)
	}
	o.Close() //nolint:errcheck
	if got := deliverCold(t, home); got != 0 {
		t.Errorf("the delivery loaded chunks %d times, want none", got)
	}
	// One unseen message read, one unseen delivered: as many as before.
	if got := coldStatus(t, home).Unseen; got != before {
		t.Errorf("UNSEEN %d after one message seen and one delivered, want %d", got, before)
	}
}

// A journal from a build without the absolute counters, after a flag change:
// the counters come out right, and loading what the change touched costs at
// most log2(chunks) + 2 chunk loads.
func TestAJournalWithoutCountersLoadsWhatItTouched(t *testing.T) {
	const n = 8 * mailindex.ChunkRecords
	home := pendingFolder(t, n)
	o, of := acceptOpen(t, home)
	ofs, err := o.(*userHandle).ui.state(of.ID)
	if err != nil {
		t.Fatal(err)
	}
	uid := uint32(5*mailindex.ChunkRecords + 2)
	ofs.mu.Lock()
	rec := encLogRec(mailindex.TxTypeFlagUpdate, 0, mailindex.EncodeTxFlagUpdatePayload([]mailindex.TxFlagUpdate{{
		UID1: uid, UID2: uid, RemoveFlags: mailindex.FlagSeen,
	}}))
	aerr := ofs.appendMutLog(rec)
	ofs.mu.Unlock()
	o.Close() //nolint:errcheck
	if aerr != nil {
		t.Fatal(aerr)
	}
	opens := mailindex.LazyOpens()
	f := coldStatus(t, home)
	loads := mailindex.LazyOpens() - opens
	if f.Unseen != 1 {
		t.Errorf("UNSEEN %d after one message unseen, want 1", f.Unseen)
	}
	if bound := int64(bits.Len(uint(n/mailindex.ChunkRecords))) + 2; loads > bound {
		t.Errorf("%d chunk loads, bound %d", loads, bound)
	}
}

// The first journal record of a message is its \Seen change: the cold
// reader's counters are right without the record.
func TestASeenChangeAsTheFirstRecordOfAMessage(t *testing.T) {
	const n = 3 * mailindex.ChunkRecords
	home := pendingFolder(t, n)
	o, of := acceptOpen(t, home)
	if err := o.UpdateFlags(of.ID, 7, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := o.OptimizeIndex(of.ID); err != nil {
		t.Fatal(err)
	}
	if err := o.UpdateFlags(of.ID, 7, []string{`\Seen`}, nil); err != nil {
		t.Fatal(err)
	}
	o.Close() //nolint:errcheck
	opens := mailindex.LazyOpens()
	f := coldStatus(t, home)
	if f.Unseen != 0 {
		t.Errorf("UNSEEN %d, want 0", f.Unseen)
	}
	if got := mailindex.LazyOpens() - opens; got != 0 {
		t.Errorf("STATUS loaded chunks %d times, want none", got)
	}
}

// A delivery after a cache purge reads no chunk (from #2635).
func TestADeliveryAfterACachePurgeReadsNoChunk(t *testing.T) {
	home := pendingFolder(t, 3*mailindex.ChunkRecords)
	o, of := acceptOpen(t, home)
	u := o.(*userHandle).ui
	if err := u.SetCacheOffsets(of.ID, map[uint32]mailbox.CacheStamp{1: {Offset: 64, CRC: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := u.PurgeCache(of.ID); err != nil {
		t.Fatal(err)
	}
	o.Close() //nolint:errcheck
	if got := deliverCold(t, home); got != 0 {
		t.Errorf("the delivery after a purge loaded chunks %d times, want none", got)
	}
}
