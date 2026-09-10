package idxrebuild_test

import (
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/idxrebuild"
	fileidx "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A record whose tail was read eight bytes early is rebuilt from the message
// itself: size, storage key and guid all come back (#1770).
//
// The damaged shape is the one found on disk -- the size is the record's own
// map_uid, the save_date is zero, and the guid holds the halves of two other
// fields -- so the repair is asserted against a real message, not a fixture of
// its own making.
func TestARepairRebuildsAShiftedTailFromTheMessage(t *testing.T) {
	root := t.TempDir()
	const user = "u@x.com"
	info := &mailbox.UserInfo{Username: user, Home: home(root, user)}

	store := mdbox.New().OpenUser(info)
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	idx := fileidx.New().OpenUser(info)
	box := mailboxbase.Open(store, idx)
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}

	const body = "Subject: one\r\n\r\nthe message itself\r\n"
	name, vsize, guid, err := store.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	meta := &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid}
	if err := box.RecordSaved(folder, "INBOX", name, meta); err != nil {
		t.Fatal(err)
	}

	// The damage: the tail as a reader eight bytes early would have stored it.
	repairer := idx.(mailbox.TailRepairer)
	if _, err := repairer.RepairRecordTails(folder.ID, map[uint32]mailbox.RecordTail{
		meta.UID: {MapUID: meta.MapUID, SaveDate: 0, VSize: meta.MapUID},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil || len(before) != 1 {
		t.Fatalf("setup: %d records, %v", len(before), err)
	}
	if !idxrebuild.ShiftedTail(before[0]) {
		t.Fatalf("the damaged record does not read as shifted: %+v", before[0])
	}

	stored, err := idxrebuild.StoredTails(box)
	if err != nil {
		t.Fatal(err)
	}
	st, err := idxrebuild.RepairShiftedTails(box, folder, stored)
	if err != nil {
		t.Fatal(err)
	}
	if st.Shifted != 1 || st.Repaired != 1 || st.Checked != 1 {
		t.Fatalf("the pass reports %+v, want checked=1 shifted=1 repaired=1", st)
	}

	after, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if after[0].VSize != vsize {
		t.Errorf("the repaired record reports size %d, want %d", after[0].VSize, vsize)
	}
	if after[0].GUID != guid {
		t.Errorf("the repaired record carries guid %x, want %x", after[0].GUID, guid)
	}
	if after[0].MapUID != meta.MapUID {
		t.Errorf("the repaired record names map uid %d, want %d", after[0].MapUID, meta.MapUID)
	}
	// The stamp the message's own trailer carries, not a number of our making.
	if want := uint32(stored[meta.MapUID].InternalDate.Unix()); after[0].SaveDate != want {
		t.Errorf("the repaired record is dated %d, want %d -- the message's own stamp",
			after[0].SaveDate, want)
	}
	if idxrebuild.ShiftedTail(after[0]) {
		t.Error("the repaired record still reads as shifted, so a rerun would never reach zero")
	}
}

// A healthy account is left alone: the mark has to be tight enough that an
// ordinary message is never rewritten.
func TestARepairLeavesHealthyRecordsAlone(t *testing.T) {
	root := t.TempDir()
	const user = "clean@x.com"
	info := &mailbox.UserInfo{Username: user, Home: home(root, user)}

	store := mdbox.New().OpenUser(info)
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	idx := fileidx.New().OpenUser(info)
	box := mailboxbase.Open(store, idx)
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"Subject: a\r\n\r\none\r\n", "Subject: b\r\n\r\ntwo\r\n"} {
		name, vsize, guid, serr := store.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, [16]byte{})
		if serr != nil {
			t.Fatal(serr)
		}
		if err := box.RecordSaved(folder, "INBOX", name, &mailbox.MessageMeta{
			Size: uint32(len(body)), VSize: vsize, GUID: guid,
		}); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := idxrebuild.StoredTails(box)
	if err != nil {
		t.Fatal(err)
	}
	st, err := idxrebuild.RepairShiftedTails(box, folder, stored)
	if err != nil {
		t.Fatal(err)
	}
	if st.Checked != 2 || st.Shifted != 0 || st.Repaired != 0 {
		t.Fatalf("the pass reports %+v over healthy records, want checked=2 and nothing else", st)
	}
}
