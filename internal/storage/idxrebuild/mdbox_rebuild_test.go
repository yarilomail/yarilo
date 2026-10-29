package idxrebuild_test

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/idxrebuild"
	fileidx "github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/mdbox/mdboxmap"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// Over mdbox a rebuild keeps the uid of a message the index knew, and a message
// it did not know comes back under a new uid carrying its storage key.
func TestAnMdboxRebuildCarriesTheStorageKey(t *testing.T) {
	info := &mailbox.UserInfo{Username: "k@x.com", Home: home(t.TempDir(), "k@x.com")}
	store := mdbox.New().OpenUser(info)
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	idx := fileidx.New().OpenUser(info)
	box := mailboxbase.Open(store, idx)
	defer box.Close()
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	save := func(body string) (string, [16]byte) {
		name, _, guid, err := store.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
		if err != nil {
			t.Fatal(err)
		}
		return name, guid
	}
	knownName, knownGUID := save("Subject: known\r\n\r\nknown\r\n")
	known := &mailbox.MessageMeta{Size: 20, VSize: 20, GUID: knownGUID}
	if err := box.RecordSaved(folder, "INBOX", knownName, known); err != nil {
		t.Fatal(err)
	}
	newName, newGUID := save("Subject: new\r\n\r\nnew\r\n")

	folder, _ = idx.OpenFolder("INBOX", 1)
	st, err := idxrebuild.RebuildFolder(box, idx, folder)
	if err != nil {
		t.Fatal(err)
	}
	if st.UIDsPreserved != 1 || st.UIDsAssigned != 1 {
		t.Fatalf("stats %+v, want one kept and one assigned", st)
	}
	msgs, _ := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	byGUID := map[[16]byte]*mailbox.MessageMeta{}
	for _, m := range msgs {
		byGUID[m.GUID] = m
	}
	if m := byGUID[knownGUID]; m == nil || m.UID != known.UID {
		t.Errorf("the known message %+v, want uid %d", m, known.UID)
	}
	want, _ := strconv.ParseUint(newName, 10, 32)
	if m := byGUID[newGUID]; m == nil || m.MapUID != uint32(want) || m.UID <= known.UID {
		t.Errorf("the new message %+v, want storage key %d and a fresh uid", m, want)
	}
}

// A body whose map record is gone names no message a folder can hold: the
// rebuild drops it and counts it, and keeps the message it can place.
func TestARebuildDropsABodyItsMapNoLongerNames(t *testing.T) {
	info := &mailbox.UserInfo{Username: "o@x.com", Home: home(t.TempDir(), "o@x.com")}
	store := mdbox.New().OpenUser(info)
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	idx := fileidx.New().OpenUser(info)
	box := mailboxbase.Open(store, idx)
	defer box.Close()
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	keep, _, keepGUID, err := store.Save("INBOX", strings.NewReader("Subject: k\r\n\r\nkeep\r\n"), 0, 20, nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if err := box.RecordSaved(folder, "INBOX", keep, &mailbox.MessageMeta{GUID: keepGUID}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.Save("INBOX", strings.NewReader("Subject: o\r\n\r\norphan\r\n"), 0, 20, nil, nil, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	// Another process drops the second body's map record, as a purge cut short would.
	keepUID, _ := strconv.ParseUint(keep, 10, 32)
	m, err := mdboxmap.Open(filepath.Join(info.Home, "mdbox", "storage"), info.Username)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ExpungeVanished(map[uint32]bool{uint32(keepUID): true}); err != nil {
		t.Fatal(err)
	}
	_ = m.Close()

	folder, _ = idx.OpenFolder("INBOX", 1)
	st, err := idxrebuild.RebuildFolder(box, idx, folder)
	if err != nil {
		t.Fatal(err)
	}
	if st.OrphansDropped != 1 || st.UIDsPreserved != 1 {
		t.Errorf("stats %+v, want the orphan dropped and the known message kept", st)
	}
}
