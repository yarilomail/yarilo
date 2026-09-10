package file

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A repaired guid reaches a base that predates the guid field: written into the
// record map alone it would be dropped by the flush (#1770).
func TestARepairedGuidReachesABasePredatingTheField(t *testing.T) {
	dir := t.TempDir()
	a := openIdx(dir, testUser)
	f, err := a.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Size: 10, MapUID: 15014}); err != nil {
		t.Fatal(err)
	}
	if err := a.withFolder(f.ID, func(fs *folderState) error { return fs.flush() }); err != nil {
		t.Fatal(err)
	}
	a.Close() //nolint:errcheck
	stripExt(t, filepath.Join(testHome(dir, testUser), "yarilo.index"), extNameGUID)

	b := openIdx(dir, testUser)
	fb, err := b.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	want := [16]byte{0xcf, 0x9a, 0x26, 0xcc, 0x1a, 0x37, 0x85, 0x06,
		0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}
	n, err := b.RepairRecordTails(fb.ID, map[uint32]mailbox.RecordTail{
		1: {MapUID: 15014, SaveDate: 1788764634, VSize: 379, GUID: want},
	})
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if n != 1 {
		t.Fatalf("the repair reports %d records, want 1", n)
	}
	b.Close() //nolint:errcheck

	c := openIdx(dir, testUser)
	defer c.Close() //nolint:errcheck
	fc, err := c.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := c.GetMessages(fc.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("the folder holds %d records, want 1", len(msgs))
	}
	if msgs[0].GUID != want {
		t.Errorf("the repaired record reads back guid %x, want %x", msgs[0].GUID, want)
	}
	if msgs[0].VSize != 379 || msgs[0].SaveDate != 1788764634 {
		t.Errorf("the repaired record reads back size %d dated %d, want 379 and 1788764634",
			msgs[0].VSize, msgs[0].SaveDate)
	}
}

// A torn intro stops the replay rather than leaving every append after it to be
// read at the old width (#1770).
func TestATornExtensionIntroStopsTheLog(t *testing.T) {
	dir := t.TempDir()
	a := openIdx(dir, testUser)
	f, err := a.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Size: 10}); err != nil {
		t.Fatal(err)
	}
	// Half an intro: framed as a whole record, too short to name its extension.
	if err := a.withFolder(f.ID, func(fs *folderState) error {
		return fs.appendMutLog(encLogRec(mailindex.TxTypeExtIntro, 0, make([]byte, 8)))
	}); err != nil {
		t.Fatal(err)
	}
	a.Close() //nolint:errcheck

	b := openIdx(dir, testUser)
	defer b.Close() //nolint:errcheck
	_, err = b.OpenFolder("INBOX", 0, "")
	if err == nil {
		t.Fatal("a torn extension intro was replayed in silence")
	}
	if got := err.Error(); !strings.Contains(got, "torn extension intro") {
		t.Errorf("the error is %q and names no torn intro", got)
	}
}
