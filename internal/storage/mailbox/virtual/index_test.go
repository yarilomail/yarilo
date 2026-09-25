package virtual

import (
	"os"
	"path/filepath"
	"testing"
)

// A backing folder is remembered by its GUID, so a rename leaves the mapping
// alone: the records name an id, and the id names a message store (#1995).
func TestRenamingABackingFolderKeepsItsID(t *testing.T) {
	dir := t.TempDir()
	guid := [16]byte{1, 2, 3}
	ix := &Index{}
	id := ix.Assign(guid, "Archive/2026", 7)
	ix.Records = []Record{{MailboxID: id, RealUID: 11}}
	if err := ix.Save(dir); err != nil {
		t.Fatal(err)
	}

	back, err := LoadIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The same folder under a new name: the id must not move.
	if got := back.Assign(guid, "Archive/2026-renamed", 7); got != id {
		t.Errorf("the renamed folder took id %d, want %d: its records would name another store", got, id)
	}
	if len(back.Boxes) != 1 {
		t.Errorf("the renamed folder was added a second time: %+v", back.Boxes)
	}
	if back.Records[0].MailboxID != id || back.Records[0].RealUID != 11 {
		t.Errorf("record read back as %+v", back.Records[0])
	}
	if back.Boxes[0].Name != "Archive/2026-renamed" {
		t.Errorf("the name was not updated: %q", back.Boxes[0].Name)
	}
}

// Ids are never reused: a folder that leaves does not hand its number to the
// next one, or a stale record would name the wrong store.
func TestMailboxIDsAreNeverReused(t *testing.T) {
	ix := &Index{}
	first := ix.Assign([16]byte{1}, "A", 1)
	second := ix.Assign([16]byte{2}, "B", 1)
	ix.Boxes = ix.Boxes[:1] // B leaves the set
	third := ix.Assign([16]byte{3}, "C", 1)
	if third == first || third == second {
		t.Errorf("the new folder took id %d, already used (%d, %d)", third, first, second)
	}
}

// A changed configuration is a differently defined set: the index is rebuilt
// rather than extended, which is what the crc in the header is for.
func TestChangedRulesAskForARebuild(t *testing.T) {
	ix := &Index{Header: Header{SearchArgsCRC32: 0xdeadbeef}}
	if ix.NeedsRebuild(0xdeadbeef) {
		t.Error("an unchanged configuration asked for a rebuild")
	}
	if !ix.NeedsRebuild(0x1234) {
		t.Error("a changed configuration did not ask for a rebuild")
	}
}

func TestIndexRefusesWhatIsNotItsOwn(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, IndexFileName), []byte("not ours at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIndex(dir); err == nil {
		t.Error("a foreign file was read as an index")
	}
}
