package file

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A backing folder is remembered by its GUID, so a rename leaves the mapping
// alone: a record names an id, and the id names a message store (#1995).
func TestVirtualBackingKeptByGUIDAcrossRename(t *testing.T) {
	b := openIdx(t.TempDir(), testUser)
	defer b.Close() //nolint:errcheck
	f, _ := b.OpenFolder("Virtual", 1, "")

	var h mailbox.VirtualHeader
	guid := [16]byte{1, 2, 3}
	id := h.AssignBacking(guid, "Archive/2026", 7)
	h.SearchCRC32 = 0xabcd
	if err := b.SetVirtualHeader(f.ID, h); err != nil {
		t.Fatal(err)
	}

	back, ok := b.VirtualHeader(f.ID)
	if !ok {
		t.Fatal("the index says nothing about the set it holds")
	}
	if got := back.AssignBacking(guid, "Archive/2026-renamed", 7); got != id {
		t.Errorf("the renamed folder took id %d, want %d: its records would name another store", got, id)
	}
	if len(back.Backing) != 1 {
		t.Errorf("the renamed folder was added a second time: %+v", back.Backing)
	}
	if back.SearchCRC32 != 0xabcd {
		t.Errorf("the rules' crc came back as %#x", back.SearchCRC32)
	}
}

// Ids are never reused: a folder that leaves does not hand its number to the
// next one, or a record written before would name the wrong store.
func TestVirtualBackingIDsAreNeverReused(t *testing.T) {
	var h mailbox.VirtualHeader
	first := h.AssignBacking([16]byte{1}, "A", 1)
	second := h.AssignBacking([16]byte{2}, "B", 1)
	h.Backing = h.Backing[:1] // B leaves the set
	third := h.AssignBacking([16]byte{3}, "C", 1)
	if third == first || third == second {
		t.Errorf("the new folder took id %d, already used (%d, %d)", third, first, second)
	}
}

// A changed configuration is a differently defined set: the mailbox is rebuilt
// rather than extended, which is what the crc in the header is for.
func TestVirtualRebuildOnChangedRules(t *testing.T) {
	h := mailbox.VirtualHeader{SearchCRC32: 0xdeadbeef}
	if h.NeedsRebuild(0xdeadbeef) {
		t.Error("an unchanged configuration asked for a rebuild")
	}
	if !h.NeedsRebuild(0x1234) {
		t.Error("a changed configuration did not ask for a rebuild")
	}
}

// The record says where the message really is, and it survives the round trip
// through the index the mailbox already keeps.
func TestVirtualRecordNamesTheBackingCopy(t *testing.T) {
	b := openIdx(t.TempDir(), testUser)
	defer b.Close() //nolint:errcheck
	f, _ := b.OpenFolder("Virtual", 1, "")

	var h mailbox.VirtualHeader
	id := h.AssignBacking([16]byte{9}, "Archive", 3)
	if err := b.SetVirtualHeader(f.ID, h); err != nil {
		t.Fatal(err)
	}
	modseq, _ := b.NextModSeq(f.ID)
	if err := b.AppendMessage(f.ID, &mailbox.MessageMeta{
		UID: 1, ModSeq: modseq, GUID: [16]byte{7},
		VirtualBacking: id, VirtualRealUID: 4242,
	}); err != nil {
		t.Fatal(err)
	}

	msgs, err := b.GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("read back %d records: %v", len(msgs), err)
	}
	if msgs[0].VirtualBacking != id || msgs[0].VirtualRealUID != 4242 {
		t.Errorf("the record came back as backing=%d real_uid=%d, want %d and 4242",
			msgs[0].VirtualBacking, msgs[0].VirtualRealUID, id)
	}
}

// A folder that is not virtual says nothing, rather than reading as one whose
// set is empty.
func TestAPlainFolderHasNoVirtualHeader(t *testing.T) {
	b := openIdx(t.TempDir(), testUser)
	defer b.Close() //nolint:errcheck
	f, _ := b.OpenFolder("INBOX", 1, "")
	if _, ok := b.VirtualHeader(f.ID); ok {
		t.Error("a plain folder answered as a virtual mailbox")
	}
}

// The header is a format on disk, so it is judged field by field after a real
// write and read: two folders, both named, with a modseq and a next uid.
func TestVirtualHeaderSurvivesItsOwnRoundTrip(t *testing.T) {
	b := openIdx(t.TempDir(), testUser)
	defer b.Close() //nolint:errcheck
	f, _ := b.OpenFolder("Virtual", 1, "")

	want := mailbox.VirtualHeader{
		SearchCRC32: 0x0f0f0f0f,
		Backing: []mailbox.VirtualBacking{
			{ID: 1, GUID: [16]byte{1, 2, 3}, Name: "Archive/2026", UIDValidity: 7, NextUID: 91, HighestModSeq: 1 << 33},
			{ID: 2, GUID: [16]byte{9, 9}, Name: "Sent", UIDValidity: 11, NextUID: 5, HighestModSeq: 42},
		},
		HighestBackingID: 2,
	}
	if err := b.SetVirtualHeader(f.ID, want); err != nil {
		t.Fatal(err)
	}
	got, ok := b.VirtualHeader(f.ID)
	if !ok {
		t.Fatal("the header written a moment ago reads as absent")
	}
	if got.SearchCRC32 != want.SearchCRC32 || got.HighestBackingID != want.HighestBackingID {
		t.Errorf("crc/highest id = %#x/%d, want %#x/%d",
			got.SearchCRC32, got.HighestBackingID, want.SearchCRC32, want.HighestBackingID)
	}
	if len(got.Backing) != len(want.Backing) {
		t.Fatalf("read back %d folders, want %d", len(got.Backing), len(want.Backing))
	}
	for i := range want.Backing {
		if got.Backing[i] != want.Backing[i] {
			t.Errorf("folder %d came back as %+v, want %+v", i, got.Backing[i], want.Backing[i])
		}
	}
}
