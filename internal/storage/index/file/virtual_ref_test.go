package file

import (
	"encoding/binary"
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// encodeRefVirtualHdr writes the reference's header (virtual-storage.h:18-42):
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
