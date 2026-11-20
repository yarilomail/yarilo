package file

import (
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
)

// asUnorderedBase rewrites the folder's base as 2.4.7 could write it: the
// records in another order than their uids, and a lineage header ending at
// the digest, with no word saying the records are in order.
func asUnorderedBase(t *testing.T, home string) {
	t.Helper()
	path := indexPathFor(home)
	f, err := mailindex.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	recs := f.Records
	for i, j := 0, len(recs)-1; i < j; i, j = i+1, j-1 {
		recs[i], recs[j] = recs[j], recs[i]
	}
	for i := range f.Extensions {
		if f.Extensions[i].Name == extNameLineage {
			f.Extensions[i].HdrData = f.Extensions[i].HdrData[:lineageOrderedSize]
			f.Extensions[i].HdrSize = lineageOrderedSize
		}
	}
	layout, err := mailindex.ComputeRecordLayout(f.Extensions)
	if err != nil {
		t.Fatal(err)
	}
	extBytes, err := mailindex.EncodeExtHeaders(layout.Extensions)
	if err != nil {
		t.Fatal(err)
	}
	hdr := f.Header
	hdr.HeaderSize = uint32(hdr.BaseHeaderSize) + uint32(len(extBytes))
	if _, err := mailindex.Recreate(mailindex.RecreateInput{Path: path, Header: hdr, Extensions: layout.Extensions, Records: recs}); err != nil {
		t.Fatal(err)
	}
	baseImages.Delete(path)
}

// A base whose records are out of uid order, as 2.4.7 wrote it, is read whole
// and ordered before it is searched: every uid is found, a flag change and an
// expunge land, the counters are right, and the next base write puts it on
// disk in order, marked (#2632).
func TestABaseOutOfUIDOrderIsOrderedBeforeItIsSearched(t *testing.T) {
	const n = 3*mailindex.ChunkRecords + 7
	home := movedFolder(t, n)
	asUnorderedBase(t, home)
	if got := mustLineage(t, home).Flags; got&lineageInUIDOrder != 0 {
		t.Fatal("the fixture base still says it is ordered")
	}

	h, f := acceptOpen(t, home)
	defer h.Close() //nolint:errcheck
	for _, uid := range []uint32{1, 2, mailindex.ChunkRecords + 3, n} {
		if m := metaOfIdx(t, h, f.ID, uid); m == nil || m.UID != uid {
			t.Fatalf("uid %d not found: %+v", uid, m)
		}
	}
	if err := h.UpdateFlags(f.ID, mailindex.ChunkRecords+3, []string{`\Flagged`}, nil); err != nil {
		t.Fatal(err)
	}
	tx, err := h.Begin(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx.Expunge(5)
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if m := metaOfIdx(t, h, f.ID, mailindex.ChunkRecords+3); m == nil || len(m.Flags) == 0 || m.Flags[len(m.Flags)-1] != `\Flagged` {
		t.Errorf("the flag change did not land: %+v", m)
	}
	if m := metaOfIdx(t, h, f.ID, 5); m != nil {
		t.Error("the expunge removed nothing")
	}
	again, err := h.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	if again.Messages != n-1 {
		t.Errorf("%d messages after one expunge of %d", again.Messages, n)
	}

	if err := h.OptimizeIndex(f.ID); err != nil {
		t.Fatal(err)
	}
	if got := mustLineage(t, home).Flags; got&lineageInUIDOrder == 0 {
		t.Error("the base written after it is not marked as in order")
	}
	disk, err := mailindex.Open(indexPathFor(home))
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(disk.Records); i++ {
		if disk.Records[i].UID <= disk.Records[i-1].UID {
			t.Fatalf("on disk record %d has uid %d after %d", i, disk.Records[i].UID, disk.Records[i-1].UID)
		}
	}
	if len(disk.Records) != n-1 {
		t.Errorf("%d records on disk, want %d", len(disk.Records), n-1)
	}
}

func mustLineage(t *testing.T, home string) lineageHdr {
	t.Helper()
	h, err := peekLineage(indexPathFor(home))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// A writer on decoded records, as a new or converted folder's is, writes its
// base in uid order too.
func TestADecodedWriterWritesItsBaseInOrder(t *testing.T) {
	const n = 2*mailindex.ChunkRecords + 3
	home := movedFolder(t, n)
	asUnorderedBase(t, home)
	writerDecoded = true
	defer func() { writerDecoded = false }()
	h, f := acceptOpen(t, home)
	defer h.Close() //nolint:errcheck
	if err := h.OptimizeIndex(f.ID); err != nil {
		t.Fatal(err)
	}
	disk, err := mailindex.Open(indexPathFor(home))
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(disk.Records); i++ {
		if disk.Records[i].UID <= disk.Records[i-1].UID {
			t.Fatalf("on disk record %d has uid %d after %d", i, disk.Records[i].UID, disk.Records[i-1].UID)
		}
	}
	if got := mustLineage(t, home).Flags; got&lineageInUIDOrder == 0 {
		t.Error("the base is not marked as in order")
	}
}
