package idxrebuild_test

import (
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/storage/idxrebuild"
	fileidx "github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// shiftedRecord saves one message to mdbox and shifts its record's tail, as the
// defect left it; it returns the record's key and its uid.
func shiftedRecord(t *testing.T, recordDate time.Time) (*mailboxbase.Box, *mailbox.Folder, uint32) {
	t.Helper()
	info := &mailbox.UserInfo{Username: "s@x.com", Home: home(t.TempDir(), "s@x.com")}
	store := mdbox.New().OpenUser(info)
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	idx := fileidx.New().OpenUser(info)
	box := mailboxbase.Open(store, idx)
	t.Cleanup(box.Close)
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	const body = "Subject: one\r\n\r\nbody\r\n"
	name, _, guid, err := store.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	meta := &mailbox.MessageMeta{Size: uint32(len(body)), VSize: uint32(len(body)), GUID: guid, InternalDate: recordDate}
	if err := box.RecordSaved(folder, "INBOX", name, meta); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.(mailbox.TailRepairer).RepairRecordTails(folder.ID, map[uint32]mailbox.RecordTail{
		meta.UID: {MapUID: 77, SaveDate: 0, VSize: 77, GUID: guid},
	}); err != nil {
		t.Fatal(err)
	}
	return box, folder, 77
}

// A tail is rebuilt only from what the store can vouch for: a size, and a date
// from the message or else the record; with neither the record is left.
func TestATailIsRebuiltOnlyFromWhatTheStoreVouchesFor(t *testing.T) {
	when := time.Date(2025, 4, 5, 6, 7, 8, 0, time.UTC)
	for name, c := range map[string]struct {
		stored     mailbox.ScanRecord
		recordDate time.Time
		repaired   int
	}{
		"no size":                {mailbox.ScanRecord{InternalDate: when}, when, 0},
		"no date on the message": {mailbox.ScanRecord{VSize: 30}, when, 1},
		"no date anywhere":       {mailbox.ScanRecord{VSize: 30}, time.Time{}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			box, folder, key := shiftedRecord(t, c.recordDate)
			st, err := idxrebuild.RepairShiftedTails(box, box.Index(), folder, map[uint32]mailbox.ScanRecord{key: c.stored})
			if err != nil {
				t.Fatal(err)
			}
			if st.Shifted != 1 || st.Repaired != c.repaired || st.Skipped != 1-c.repaired {
				t.Errorf("the pass reports %+v, want %d repaired", st, c.repaired)
			}
		})
	}
}

// A store that scans one folder at a time has no storage-wide tails to offer.
func TestAFolderScannedStoreOffersNoTails(t *testing.T) {
	info := &mailbox.UserInfo{Username: "m@x.com", Home: home(t.TempDir(), "m@x.com")}
	store := maildir.New().OpenUser(info)
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	box := mailboxbase.Open(store, fileidx.New().OpenUser(info))
	defer box.Close()
	stored, err := idxrebuild.StoredTails(box, box.Index())
	if err != nil || stored != nil {
		t.Errorf("tails %v, %v; want none", stored, err)
	}
}
