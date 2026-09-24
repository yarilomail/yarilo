package file

import (
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func guidTxFolder(t *testing.T) (*userIndex, string, uint64) {
	t.Helper()
	home := t.TempDir()
	ui := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	f, err := ui.OpenFolder("INBOX", 7, "")
	if err != nil {
		t.Fatal(err)
	}
	return ui, home, f.ID
}

// Every copy a command writes reaches the store, in the same commit as the
// folder's own write (#1711, INTERNALS.md §35).
func TestACommittedAppendRecordsTheCopy(t *testing.T) {
	ui, _, folderID := guidTxFolder(t)
	tx, err := ui.Begin(folderID)
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{GUID: [16]byte{7, 7}, InternalDate: time.Unix(1700000000, 0)}
	tx.Append(m)
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	got, err := ui.GUIDRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("the store holds %d copies after one append", len(got))
	}
	if got[0].GUID != m.GUID || got[0].FolderGUID != folderGUIDOf(t, ui, folderID) || got[0].UID != m.UID {
		t.Errorf("the copy reads %+v, the message is guid %x in folder %d uid %d",
			got[0], m.GUID, folderID, m.UID)
	}
}

// An expunge removes the copy, not the message: another folder's copy of the
// same GUID stays.
func TestACommittedExpungeRemovesOnlyThatCopy(t *testing.T) {
	ui, _, folderID := guidTxFolder(t)
	other, err := ui.OpenFolder("Archive", 7, "")
	if err != nil {
		t.Fatal(err)
	}
	guid := [16]byte{3, 1, 4}
	inbox := &mailbox.MessageMeta{GUID: guid, InternalDate: time.Unix(1700000000, 0)}
	kept := &mailbox.MessageMeta{GUID: guid, InternalDate: time.Unix(1700000000, 0)}
	for _, pair := range []struct {
		id uint64
		m  *mailbox.MessageMeta
	}{{folderID, inbox}, {other.ID, kept}} {
		tx, berr := ui.Begin(pair.id)
		if berr != nil {
			t.Fatal(berr)
		}
		tx.Append(pair.m)
		if _, cerr := tx.Commit(); cerr != nil {
			t.Fatal(cerr)
		}
	}

	tx, err := ui.Begin(folderID)
	if err != nil {
		t.Fatal(err)
	}
	tx.Expunge(inbox.UID)
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	got, err := ui.GUIDRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("the store holds %d copies, want the one that was not expunged: %+v", len(got), got)
	}
	if got[0].FolderGUID != folderGUIDOf(t, ui, other.ID) || got[0].UID != kept.UID {
		t.Errorf("the surviving copy is %+v, want folder %d uid %d", got[0], other.ID, kept.UID)
	}
}

// A folder write that is refused leaves the store without the copy: the store
// follows the folder, it does not lead it.
func TestARefusedFolderWriteLeavesNoCopy(t *testing.T) {
	ui, _, folderID := guidTxFolder(t)
	// Warm the store so its own file exists: this row is about the copy, not
	// about a store that could not be created.
	if err := ui.AppendGUIDRecord(mailbox.GUIDRecord{GUID: [16]byte{1}, FolderGUID: [16]byte{9, 9, 9}, UID: 1}); err != nil {
		t.Fatal(err)
	}
	before, err := ui.GUIDRecords()
	if err != nil {
		t.Fatal(err)
	}

	// A log that cannot be opened at all: the path is a directory, so the
	// folder's write is refused where it appends.
	ui.mu.Lock()
	fs := ui.open[folderID]
	ui.mu.Unlock()
	fs.mu.Lock()
	logPath := fs.indexPath + ".log"
	if fs.logFD != nil {
		_ = fs.logFD.Close()
		fs.logFD = nil
	}
	fs.mu.Unlock()
	if err := os.RemoveAll(logPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(logPath, 0o700); err != nil {
		t.Fatal(err)
	}

	tx, err := ui.Begin(folderID)
	if err != nil {
		t.Fatal(err)
	}
	tx.Append(&mailbox.MessageMeta{GUID: [16]byte{2, 2}, InternalDate: time.Unix(1700000001, 0)})
	if _, err := tx.Commit(); err == nil {
		t.Fatal("the folder write was not refused, so this row proves nothing")
	}

	after, err := ui.GUIDRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("the store took %d copies from a transaction that failed", len(after)-len(before))
	}
}

// One transaction takes the store once, whatever it holds: a hold per record
// is what the folder's own rule forbids (#1827).
func TestATransactionTakesTheStoreOnce(t *testing.T) {
	ui, _, folderID := guidTxFolder(t)
	tx, err := ui.Begin(folderID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		tx.Append(&mailbox.MessageMeta{GUID: [16]byte{byte(i + 1)}, InternalDate: time.Unix(1700000000, 0)})
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	before := guidHolds(t)
	tx, err = ui.Begin(folderID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		tx.Append(&mailbox.MessageMeta{GUID: [16]byte{byte(i + 100)}, InternalDate: time.Unix(1700000000, 0)})
	}
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if took := guidHolds(t) - before; took != 1 {
		t.Errorf("a transaction over 8 copies took the store %d times, want 1", took)
	}
}

// An expunge in a mailbox that never wrote a copy does not create the store:
// a user nobody addresses by GUID pays nothing for it.
func TestAnExpungeDoesNotCreateTheStore(t *testing.T) {
	ui, _, folderID := guidTxFolder(t)
	m := &mailbox.MessageMeta{Size: 10}
	if err := ui.AllocateAndAppend(folderID, m); err != nil {
		t.Fatal(err)
	}
	tx, err := ui.Begin(folderID)
	if err != nil {
		t.Fatal(err)
	}
	tx.Expunge(m.UID)
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ui.GUIDStorePath()); err == nil {
		t.Error("an expunge created a GUID store that no copy was ever written to")
	}
}

func guidHolds(t *testing.T) int {
	t.Helper()
	fams, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, f := range fams {
		if f.GetName() != "fileindex_lock_acquired_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "site" && l.GetValue() == lockSiteGUIDAppend {
					total += m.GetCounter().GetValue()
				}
			}
		}
	}
	return int(total)
}

// folderGUIDOf reads a folder's own identity, which is what the store records.
func folderGUIDOf(t *testing.T, ui *userIndex, folderID uint64) [16]byte {
	t.Helper()
	var out [16]byte
	if err := ui.withFolderROUnlocked(folderID, func(fs *folderState) error {
		out = fs.hdr.MailboxGUID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// The folder a copy names must still be that folder in the next process: the
// index id is assigned in open order, so the second handle reverses it.
func TestACopyNamesTheSameFolderInANewProcess(t *testing.T) {
	home := t.TempDir()
	first := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	inbox, err := first.OpenFolder("INBOX", 7, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.OpenFolder("Archive", 7, ""); err != nil {
		t.Fatal(err)
	}
	tx, err := first.Begin(inbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{GUID: [16]byte{5, 5}, InternalDate: time.Unix(1700000000, 0)}
	tx.Append(m)
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	wantFolder := folderGUIDOf(t, first, inbox.ID)

	// A new handle, folders opened in the other order: the ids are swapped.
	second := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	if _, err := second.OpenFolder("Archive", 7, ""); err != nil {
		t.Fatal(err)
	}
	reopened, err := second.OpenFolder("INBOX", 7, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := second.GUIDRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("the store holds %d copies after reopening", len(got))
	}
	if got[0].FolderGUID != wantFolder || got[0].FolderGUID != folderGUIDOf(t, second, reopened.ID) {
		t.Errorf("the copy names folder %x, INBOX is %x", got[0].FolderGUID, wantFolder)
	}
}

// A store written in the older record shape is refused rather than read as
// truth: it is derived, and a rebuild replaces it.
func TestAStoreInTheOlderShapeIsRefused(t *testing.T) {
	home := t.TempDir()
	ui := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	f, err := ui.OpenFolder("INBOX", 7, "")
	if err != nil {
		t.Fatal(err)
	}
	// Written as #1994 wrote it: the same extension, eight bytes shorter.
	exts := []mailindex.Extension{{
		Name: extNameGUIDMap, RecordSize: 48, RecordAlign: 8, ResetID: 1,
	}}
	layout, lerr := mailindex.ComputeRecordLayout(exts)
	if lerr != nil {
		t.Fatal(lerr)
	}
	extBytes, eerr := mailindex.EncodeExtHeaders(layout.Extensions)
	if eerr != nil {
		t.Fatal(eerr)
	}
	hdr := mailindex.NewHeader(1)
	hdr.UIDValidity = 1
	hdr.RecordSize = layout.RecordSize
	hdr.HeaderSize = uint32(mailindex.HeaderMinSize) + uint32(len(extBytes))
	if _, rerr := mailindex.Recreate(mailindex.RecreateInput{
		Path: ui.GUIDStorePath(), Header: hdr, Extensions: layout.Extensions,
	}); rerr != nil {
		t.Fatal(rerr)
	}

	again := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	stale := testutil.ToFloat64(metricGUIDStoreStale)
	got, err := again.GUIDRecords()
	if err != nil {
		t.Fatalf("an older store answered with an error rather than with nothing: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("an older store answered with %d copies, which name folders by a number", len(got))
	}
	if now := testutil.ToFloat64(metricGUIDStoreStale); now != stale+1 {
		t.Errorf("stale stores seen = %v, want %v", now, stale+1)
	}
	_ = f
}
