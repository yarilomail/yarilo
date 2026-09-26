package virtual

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A header the reference wrote names its folders; the first open gives each
// one the identity this server keeps it by, and the uids stay where they are.
func TestAdoptNamesKeepsTheUidsOfAMigratedMailbox(t *testing.T) {
	was := mailbox.VirtualHeader{
		SearchCRC32:      0xdeadbeef,
		HighestBackingID: 3,
		Backing: []mailbox.VirtualBacking{
			{ID: 1, Name: "INBOX", UIDValidity: 11, NextUID: 42, HighestModSeq: 7},
			{ID: 2, Name: "Archive/2026", UIDValidity: 12, NextUID: 5, HighestModSeq: 9},
			{ID: 3, Name: "Gone", UIDValidity: 13, NextUID: 2, HighestModSeq: 1},
		},
	}
	folders := []Backing{
		{Name: "INBOX", GUID: [16]byte{1}},
		{Name: "Archive/2026", GUID: [16]byte{2}},
	}
	got, taken := AdoptNames(was, folders)
	if !taken {
		t.Fatal("nothing was taken from a header that names its folders")
	}
	for i, want := range [][16]byte{{1}, {2}, {}} {
		if got.Backing[i].GUID != want {
			t.Errorf("folder %q took %x, want %x", got.Backing[i].Name, got.Backing[i].GUID, want)
		}
	}
	for i := range was.Backing {
		b, w := got.Backing[i], was.Backing[i]
		if b.ID != w.ID || b.UIDValidity != w.UIDValidity || b.NextUID != w.NextUID || b.HighestModSeq != w.HighestModSeq {
			t.Errorf("folder %q became %+v, want its watermarks kept %+v", w.Name, b, w)
		}
	}
	if was.Backing[0].GUID != ([16]byte{}) {
		t.Errorf("the header it was given now carries %x: the caller's copy was written through", was.Backing[0].GUID)
	}
	if got.SearchCRC32 != was.SearchCRC32 || got.HighestBackingID != was.HighestBackingID {
		t.Errorf("header became %+v, want the crc and the highest id kept", got)
	}
}

// A header this server wrote is left alone, and the one it was given is not
// written through.
func TestAdoptNamesTakesNothingFromOurOwnHeader(t *testing.T) {
	was := mailbox.VirtualHeader{Backing: []mailbox.VirtualBacking{
		{ID: 1, Name: "INBOX", GUID: [16]byte{9}, NextUID: 42},
	}}
	got, taken := AdoptNames(was, []Backing{{Name: "INBOX", GUID: [16]byte{1}}})
	if taken {
		t.Error("a header that already identifies its folders was taken from")
	}
	if got.Backing[0].GUID != [16]byte{9} || was.Backing[0].GUID != [16]byte{9} {
		t.Errorf("guid became %x (source %x), want 09 kept in both", got.Backing[0].GUID, was.Backing[0].GUID)
	}
}

// A header naming only folders that are gone takes nothing: there is no
// identity to give them, and writing it back would say there was.
func TestAdoptNamesTakesNothingWhenEveryFolderIsGone(t *testing.T) {
	was := mailbox.VirtualHeader{Backing: []mailbox.VirtualBacking{
		{ID: 1, Name: "Gone", NextUID: 9},
		{ID: 2, Name: "AlsoGone", NextUID: 4},
	}}
	got, taken := AdoptNames(was, []Backing{{Name: "INBOX", GUID: [16]byte{1}}})
	if taken {
		t.Error("a header whose folders are all gone was taken from")
	}
	for _, b := range got.Backing {
		if b.GUID != ([16]byte{}) {
			t.Errorf("folder %q took %x, and it does not exist", b.Name, b.GUID)
		}
	}
}
