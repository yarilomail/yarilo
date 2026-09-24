package file

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A rebuilt store is the store the transactions wrote: same records, same
// bytes, so nothing about a rebuild is a second format (#1711).
func TestARebuiltStoreEqualsWhatTheTransactionsWrote(t *testing.T) {
	written, wrote := guidTxFolder2(t)
	writtenBytes, err := os.ReadFile(written.GUIDStorePath())
	if err != nil {
		t.Fatal(err)
	}

	rebuiltHome := t.TempDir()
	rebuilt := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: rebuiltHome}).(*userHandle).ui
	if err := rebuilt.ReplaceGUIDStore(wrote); err != nil {
		t.Fatal(err)
	}
	rebuiltBytes, err := os.ReadFile(rebuilt.GUIDStorePath())
	if err != nil {
		t.Fatal(err)
	}

	// The header carries an index id and the write times; the records are what
	// this row is about.
	if a, b := recordsOf(t, writtenBytes), recordsOf(t, rebuiltBytes); a != b {
		t.Errorf("the rebuilt store's records differ from the written ones:\n%s\n%s", a, b)
	}
	back, err := rebuilt.GUIDRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != len(wrote) {
		t.Fatalf("the rebuilt store holds %d copies, %d were written", len(back), len(wrote))
	}
	for i := range wrote {
		if back[i] != wrote[i] {
			t.Errorf("copy %d reads %+v, want %+v", i, back[i], wrote[i])
		}
	}
}

// A rebuild replaces a store in the older shape, which is what a build that
// wrote 48-byte records left behind.
func TestARebuildReplacesAStoreInTheOlderShape(t *testing.T) {
	home := t.TempDir()
	ui := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	writeOldShapeStore(t, ui)
	if got, err := ui.GUIDRecords(); err != nil || len(got) != 0 {
		t.Fatalf("the older store answered %v (err %v) before the rebuild", got, err)
	}

	want := []mailbox.GUIDRecord{{
		GUID: [16]byte{1, 2}, FolderGUID: [16]byte{3, 4}, UID: 9, InternalDate: 1700000000,
	}}
	if err := ui.ReplaceGUIDStore(want); err != nil {
		t.Fatal(err)
	}
	got, err := ui.GUIDRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("after the rebuild the store holds %+v, want %+v", got, want)
	}
}

// A rebuild reads the folders and writes one file: no folder index is touched.
func TestARebuildDoesNotTouchTheFolders(t *testing.T) {
	ui, home, folderID := guidTxFolder(t)
	tx, err := ui.Begin(folderID)
	if err != nil {
		t.Fatal(err)
	}
	tx.Append(&mailbox.MessageMeta{GUID: [16]byte{6}, InternalDate: time.Unix(1700000000, 0)})
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	folderIndex := indexFileIn(t, home)
	before, err := os.Stat(folderIndex)
	if err != nil {
		t.Fatal(err)
	}

	copies, err := ui.GUIDRecords()
	if err != nil {
		t.Fatal(err)
	}
	if err := ui.ReplaceGUIDStore(copies); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(folderIndex)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Errorf("the rebuild wrote to a folder index: %v/%d -> %v/%d",
			before.ModTime(), before.Size(), after.ModTime(), after.Size())
	}
}

// guidTxFolder2 delivers two copies through transactions and returns both the
// index that wrote them and the copies themselves.
func guidTxFolder2(t *testing.T) (*userIndex, []mailbox.GUIDRecord) {
	t.Helper()
	ui, _, folderID := guidTxFolder(t)
	for i := 0; i < 2; i++ {
		tx, err := ui.Begin(folderID)
		if err != nil {
			t.Fatal(err)
		}
		tx.Append(&mailbox.MessageMeta{
			GUID: [16]byte{byte(i + 1)}, InternalDate: time.Unix(1700000000+int64(i), 0),
		})
		if _, err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	copies, err := ui.GUIDRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 2 {
		t.Fatalf("the transactions wrote %d copies, want 2", len(copies))
	}
	return ui, copies
}

// recordsOf renders the record area of a store file, which is what two writers
// must agree on byte for byte.
func recordsOf(t *testing.T, raw []byte) string {
	t.Helper()
	f, err := mailindex.Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	out := ""
	for _, r := range f.Records {
		out += fmt.Sprintf("%d:%x\n", r.UID, r.Ext[extNameGUIDMap])
	}
	return out
}

// writeOldShapeStore leaves behind what a build with 48-byte records wrote.
func writeOldShapeStore(t *testing.T, ui *userIndex) {
	t.Helper()
	exts := []mailindex.Extension{{
		Name: extNameGUIDMap, RecordSize: 48, RecordAlign: 8, ResetID: 1,
	}}
	layout, err := mailindex.ComputeRecordLayout(exts)
	if err != nil {
		t.Fatal(err)
	}
	extBytes, err := mailindex.EncodeExtHeaders(layout.Extensions)
	if err != nil {
		t.Fatal(err)
	}
	hdr := mailindex.NewHeader(1)
	hdr.UIDValidity = 1
	hdr.RecordSize = layout.RecordSize
	hdr.HeaderSize = uint32(mailindex.HeaderMinSize) + uint32(len(extBytes))
	if _, err := mailindex.Recreate(mailindex.RecreateInput{
		Path: ui.GUIDStorePath(), Header: hdr, Extensions: layout.Extensions,
	}); err != nil {
		t.Fatal(err)
	}
}

// indexFileIn finds the folder index this layout wrote.
func indexFileIn(t *testing.T, home string) string {
	t.Helper()
	var found string
	if err := filepath.WalkDir(home, func(path string, d os.DirEntry, werr error) error {
		if werr != nil || d.IsDir() || d.Name() != IndexFileName {
			return werr
		}
		found = path
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if found == "" {
		t.Fatal("no folder index was written")
	}
	return found
}

// A rebuild is visible to a handle that has already read the store: the map
// built from the old file must not answer for the new one.
func TestARebuildIsVisibleToAHandleThatAlreadyRead(t *testing.T) {
	ui, wrote := guidTxFolder2(t)
	if got, err := ui.GUIDCopies([][16]byte{wrote[0].GUID}); err != nil || len(got) != 1 {
		t.Fatalf("the store answered %v (err %v) before the rebuild", got, err)
	}

	replacement := []mailbox.GUIDRecord{{
		GUID: [16]byte{200}, FolderGUID: [16]byte{7}, UID: 3, InternalDate: 1700000009,
	}}
	if err := ui.ReplaceGUIDStore(replacement); err != nil {
		t.Fatal(err)
	}
	if got, err := ui.GUIDCopies([][16]byte{wrote[0].GUID}); err != nil || len(got) != 0 {
		t.Errorf("a copy the rebuild dropped is still answered: %v (err %v)", got, err)
	}
	got, err := ui.GUIDCopies([][16]byte{replacement[0].GUID})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != replacement[0] {
		t.Errorf("the rebuilt copy reads %+v, want %+v", got, replacement)
	}
}

// A copy written after a rebuild lands in the file the rebuild wrote: a handle
// still pointing at the replaced one would write into a file nobody reads.
func TestACopyAfterARebuildLandsInTheNewStore(t *testing.T) {
	ui, _, folderID := guidTxFolder(t)
	if err := ui.ReplaceGUIDStore([]mailbox.GUIDRecord{{
		GUID: [16]byte{50}, FolderGUID: [16]byte{7}, UID: 1, InternalDate: 1700000000,
	}}); err != nil {
		t.Fatal(err)
	}

	tx, err := ui.Begin(folderID)
	if err != nil {
		t.Fatal(err)
	}
	tx.Append(&mailbox.MessageMeta{GUID: [16]byte{51}, InternalDate: time.Unix(1700000001, 0)})
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Read by somebody who never saw the old file.
	again := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: ui.home}).(*userHandle).ui
	got, err := again.GUIDRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("the store holds %d copies after a rebuild and one append, want 2: %+v", len(got), got)
	}
}

// The rebuild leaves no log behind. It is not what keeps the copies right --
// a log naming another file is not folded in -- but a file nobody will read.
func TestARebuildLeavesNoLogBehind(t *testing.T) {
	ui, wrote := guidTxFolder2(t)
	logPath := ui.GUIDStorePath() + ".log"
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("the transactions left no log to begin with: %v", err)
	}
	if err := ui.ReplaceGUIDStore(wrote); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("the replaced store's log is still there: %v", err)
	}

	// And the copies are what the rebuild wrote, once each.
	again := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: ui.home}).(*userHandle).ui
	back, err := again.GUIDRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != len(wrote) {
		t.Errorf("a fresh handle sees %d copies, the rebuild wrote %d", len(back), len(wrote))
	}
}
