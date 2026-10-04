package file

import (
	"encoding/binary"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/virtual"
	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// encodeRefVirtualHdr writes the reference's header:
// the same watermarks, the folder named rather than identified.
func encodeRefVirtualHdr(crc uint32, boxes []struct {
	ID            uint32
	Name          string
	UIDValidity   uint32
	NextUID       uint32
	HighestModSeq uint64
}) []byte {
	out := make([]byte, 0, virtualHdrHead+len(boxes)*refBoxSize)
	out = binary.LittleEndian.AppendUint32(out, 7) // change counter
	out = binary.LittleEndian.AppendUint32(out, uint32(len(boxes)))
	out = binary.LittleEndian.AppendUint32(out, 9) // highest id
	out = binary.LittleEndian.AppendUint32(out, crc)
	for _, b := range boxes {
		out = binary.LittleEndian.AppendUint32(out, b.ID)
		out = binary.LittleEndian.AppendUint32(out, uint32(len(b.Name)))
		out = binary.LittleEndian.AppendUint32(out, b.UIDValidity)
		out = binary.LittleEndian.AppendUint32(out, b.NextUID)
		out = binary.LittleEndian.AppendUint64(out, b.HighestModSeq)
	}
	for _, b := range boxes {
		out = append(out, b.Name...)
	}
	return out
}

type refBox = struct {
	ID            uint32
	Name          string
	UIDValidity   uint32
	NextUID       uint32
	HighestModSeq uint64
}

// A header the reference wrote is read: the folders it names keep their ids
// and watermarks, and carry no GUID until the folders themselves are opened.
func TestAHeaderTheReferenceWroteIsRead(t *testing.T) {
	boxes := []refBox{
		{ID: 1, Name: "INBOX", UIDValidity: 11, NextUID: 42, HighestModSeq: 7},
		{ID: 2, Name: "Archive/2026", UIDValidity: 12, NextUID: 5, HighestModSeq: 9},
	}
	h, ok := decodeVirtualHdr(encodeRefVirtualHdr(0xdeadbeef, boxes))
	if !ok {
		t.Fatal("the reference's header was not read")
	}
	if h.SearchCRC32 != 0xdeadbeef || h.HighestBackingID != 9 || len(h.Backing) != 2 {
		t.Fatalf("header = %+v, want crc deadbeef, highest id 9, two folders", h)
	}
	for i, b := range h.Backing {
		w := boxes[i]
		if b.ID != w.ID || b.Name != w.Name || b.UIDValidity != w.UIDValidity ||
			b.NextUID != w.NextUID || b.HighestModSeq != w.HighestModSeq {
			t.Errorf("folder %d = %+v, want %+v", i, b, w)
		}
		if b.GUID != ([16]byte{}) {
			t.Errorf("folder %d carries a GUID %x, and the reference names no GUID", i, b.GUID)
		}
	}
}

// Ours is still read as ours, though both layouts start with four words.
func TestOurHeaderIsNotReadAsTheReferences(t *testing.T) {
	want := mailbox.VirtualHeader{
		SearchCRC32:      0x0f0f0f0f,
		HighestBackingID: 2,
		Backing: []mailbox.VirtualBacking{
			{ID: 1, GUID: [16]byte{1, 2, 3}, Name: "Archive/2026", UIDValidity: 7, NextUID: 91, HighestModSeq: 1 << 33},
			{ID: 2, GUID: [16]byte{9, 9}, Name: "Sent", UIDValidity: 11, NextUID: 5, HighestModSeq: 42},
		},
	}
	got, ok := decodeVirtualHdr(encodeVirtualHdr(want))
	if !ok {
		t.Fatal("our own header was not read")
	}
	if len(got.Backing) != len(want.Backing) {
		t.Fatalf("read %d folders, wrote %d", len(got.Backing), len(want.Backing))
	}
	for i := range want.Backing {
		if got.Backing[i] != want.Backing[i] {
			t.Errorf("folder %d = %+v, want %+v", i, got.Backing[i], want.Backing[i])
		}
	}
}

// A reference header whose crc32 reads as a small count must not parse as one
// of ours: the bytes left over are what says the layout is another.
func TestAReferenceHeaderIsNotReadAsOurs(t *testing.T) {
	boxes := []refBox{
		{ID: 1, Name: "INBOX", UIDValidity: 11, NextUID: 42, HighestModSeq: 7},
		{ID: 2, Name: "Sent", UIDValidity: 12, NextUID: 5, HighestModSeq: 9},
	}
	// crc 1 sits where ours keeps the folder count, so an ours-shaped parse
	// passes its bounds check and stops short of the end.
	h, ok := decodeVirtualHdr(encodeRefVirtualHdr(1, boxes))
	if !ok {
		t.Fatal("the reference's header was not read")
	}
	if len(h.Backing) != 2 || h.Backing[0].Name != "INBOX" || h.Backing[1].Name != "Sent" {
		t.Fatalf("read %+v, want the two folders the reference named", h.Backing)
	}
	if h.SearchCRC32 != 1 {
		t.Errorf("crc = %d, want 1", h.SearchCRC32)
	}
}

// The index a running reference wrote (2.4.5, a Virtual/All over INBOX): its
// header reads, and the folder keeps the watermarks it had there.
func TestTheIndexARunningReferenceWroteIsRead(t *testing.T) {
	f, err := mailindex.Open("testdata/reference-virtual.index")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Records) != 116 {
		t.Errorf("the fixture holds %d records, and the reference counted 116", len(f.Records))
	}
	ext := findExt(f.Extensions, extNameVirtual)
	if ext == nil {
		t.Fatal("the reference's index carries no virtual extension")
	}
	h, ok := decodeVirtualHdr(ext.HdrData)
	if !ok {
		t.Fatal("the header the reference wrote was not read")
	}
	if len(h.Backing) != 1 {
		t.Fatalf("read %d folders, and the configuration named one", len(h.Backing))
	}
	b := h.Backing[0]
	if b.Name != "INBOX" || b.ID != 1 || b.UIDValidity != 1786303502 || b.NextUID != 674 || b.HighestModSeq != 1070 {
		t.Errorf("folder = %+v, want INBOX id 1, uidvalidity 1786303502, uidnext 674, modseq 1070", b)
	}
	if b.GUID != ([16]byte{}) {
		t.Errorf("folder carries a GUID %x, and the reference names no GUID", b.GUID)
	}
	if !virtual.NeedsNames(mailbox.VirtualHeader{Backing: h.Backing}) {
		t.Error("a header the reference wrote reads as needing no names")
	}
}

// Taking over the reference's header leaves the uids alone: the folder keeps
// its id and watermarks, and only gains the identity this server knows it by.
func TestTakingOverTheReferencesHeaderKeepsTheUids(t *testing.T) {
	f, err := mailindex.Open("testdata/reference-virtual.index")
	if err != nil {
		t.Fatal(err)
	}
	was, ok := decodeVirtualHdr(findExt(f.Extensions, extNameVirtual).HdrData)
	if !ok {
		t.Fatal("the reference's header was not read")
	}
	inbox := [16]byte{7, 7, 7}
	got, taken := virtual.AdoptNames(was, []virtual.Backing{{Name: "INBOX", GUID: inbox}})
	if !taken {
		t.Fatal("nothing was taken from the reference's header")
	}
	b, w := got.Backing[0], was.Backing[0]
	if b.GUID != inbox {
		t.Errorf("folder took %x, want the identity this server knows it by", b.GUID)
	}
	if b.ID != w.ID || b.UIDValidity != w.UIDValidity || b.NextUID != w.NextUID || b.HighestModSeq != w.HighestModSeq {
		t.Errorf("folder became %+v, want the reference's watermarks kept %+v", b, w)
	}
	// Written back as ours and read again: the set is the same one, so the
	// records the reference numbered keep the uids they had.
	idx := openIdx(t.TempDir(), testUser)
	defer idx.Close() //nolint:errcheck
	fold, _ := idx.OpenFolder("Virtual", 1, "")
	if err := idx.SetVirtualHeader(fold.ID, got); err != nil {
		t.Fatal(err)
	}
	back, ok := idx.VirtualHeader(fold.ID)
	if !ok || len(back.Backing) != 1 {
		t.Fatalf("read back %+v, want the one folder", back)
	}
	if back.Backing[0] != b {
		t.Errorf("read back %+v, want %+v", back.Backing[0], b)
	}
	if virtual.NeedsNames(back) {
		t.Error("the header written back still names a folder without an identity")
	}
}
