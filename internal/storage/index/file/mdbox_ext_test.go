package file

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The reference keeps an mdbox message's storage key in a per-record extension,
// not by scanning the map: checked against its own bytes (#1700).
func TestTheMdboxRecordMatchesTheReferenceBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yarilo.index")
	if err := os.WriteFile(path, dboxref.IndexBase(t), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := mailindex.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ext := findExt(f.Extensions, extNameMdbox)
	if ext == nil {
		t.Fatalf("their index carries no %q extension: %v", extNameMdbox, extInventory(f.Extensions))
	}
	if ext.RecordSize != mdboxRecSize {
		t.Errorf("their record field is %d bytes, ours %d", ext.RecordSize, mdboxRecSize)
	}
	if len(f.Records) == 0 {
		t.Fatal("their index holds no records")
	}
	for _, rec := range f.Records {
		theirs := rec.Ext[extNameMdbox]
		if len(theirs) != mdboxRecSize {
			t.Fatalf("uid %d carries %d bytes, want %d", rec.UID, len(theirs), mdboxRecSize)
		}
		mapUID := binary.LittleEndian.Uint32(theirs[0:4])
		saveDate := binary.LittleEndian.Uint32(theirs[4:8])
		if mapUID != rec.UID {
			// Their fixture was delivered in order, so the two run together;
			// this is what says the first field is the map uid at all.
			t.Errorf("uid %d names map uid %d", rec.UID, mapUID)
		}
		if got := encodeMdboxRec(mapUID, saveDate); string(got) != string(theirs) {
			t.Errorf("uid %d: we write % x, they wrote % x", rec.UID, got, theirs)
		}
	}
}

// A message we store carries it, read back after a reopen: a field that never
// reached the base is one the resolver will not find (#1700).
func TestOurRecordCarriesTheMapUID(t *testing.T) {
	dir := t.TempDir()
	a := openIdx(dir, testUser)
	f, err := a.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AppendMessage(f.ID, &mailbox.MessageMeta{
		UID: 1, Size: 10, MapUID: 7, SaveDate: 1788000000,
	}); err != nil {
		t.Fatal(err)
	}
	a.Close() //nolint:errcheck

	b := openIdx(dir, testUser)
	defer b.Close() //nolint:errcheck
	fb, err := b.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	fs := b.folderStateFor(t, "INBOX")
	if len(fs.file.Records) != 1 {
		t.Fatalf("got %d records, want 1", len(fs.file.Records))
	}
	mapUID, saveDate := decodeMdboxRec(fs.file.Records[0].Ext[extNameMdbox])
	if mapUID != 7 || saveDate != 1788000000 {
		t.Errorf("the record says map uid %d saved %d, want 7 and 1788000000", mapUID, saveDate)
	}
	_ = fb
}

// The whole extension, not half: the geometry a reader takes the layout from is
// theirs too, header included (#1700).
func TestOurMdboxExtensionHasTheReferenceGeometry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "their.index")
	if err := os.WriteFile(path, dboxref.IndexBase(t), 0o600); err != nil {
		t.Fatal(err)
	}
	their, err := mailindex.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	theirs := findExt(their.Extensions, extNameMdbox)
	if theirs == nil {
		t.Fatal("their index carries no mdbox extension")
	}

	dir := t.TempDir()
	a := openIdx(dir, testUser)
	defer a.Close() //nolint:errcheck
	f, err := a.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AppendMessage(f.ID, &mailbox.MessageMeta{
		UID: 1, Size: 10, MapUID: 7, SaveDate: 1788000000,
	}); err != nil {
		t.Fatal(err)
	}
	fs := a.folderStateFor(t, "INBOX")
	ours := findExt(fs.file.Extensions, extNameMdbox)
	if ours == nil {
		t.Fatal("we declare no mdbox extension")
	}
	if ours.HdrSize != theirs.HdrSize || len(ours.HdrData) != len(theirs.HdrData) {
		t.Errorf("our header is %d bytes (%d of data), theirs %d (%d)",
			ours.HdrSize, len(ours.HdrData), theirs.HdrSize, len(theirs.HdrData))
	}
	if ours.RecordSize != theirs.RecordSize || ours.RecordAlign != theirs.RecordAlign {
		t.Errorf("our field is %d bytes aligned %d, theirs %d aligned %d",
			ours.RecordSize, ours.RecordAlign, theirs.RecordSize, theirs.RecordAlign)
	}
	if ours.ResetID != theirs.ResetID {
		t.Errorf("our reset id is %d, theirs %d", ours.ResetID, theirs.ResetID)
	}
}

// folderStateFor reaches the open state for a folder, so a row can read what
// the records hold rather than what a reader reports.
func (u *userIndex) folderStateFor(t *testing.T, folder string) *folderState {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, fs := range u.open {
		if fs.folder == folder {
			return fs
		}
	}
	t.Fatalf("folder %q not open", folder)
	return nil
}

// A second reader decodes a record appended right after the field was declared,
// with no base rewrite in between; the row below flushes first, and stayed green.
func TestASecondReaderDecodesARecordAppendedBeforeAnyBaseRewrite(t *testing.T) {
	dir := t.TempDir()
	a := openIdx(dir, testUser)
	f, err := a.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Size: 10}); err != nil {
		t.Fatal(err)
	}
	if err := a.withFolder(f.ID, func(fs *folderState) error { return fs.flush() }); err != nil {
		t.Fatal(err)
	}
	a.Close() //nolint:errcheck
	stripExt(t, filepath.Join(testHome(dir, testUser), "yarilo.index"), extNameMdbox)

	// The writer: the first record carrying a storage key, and nothing after it.
	b := openIdx(dir, testUser)
	defer b.Close() //nolint:errcheck
	fb, err := b.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	want := mailbox.MessageMeta{
		UID: 2, Size: 379, VSize: 379, MapUID: 15014, SaveDate: 1788764634,
		GUID: [16]byte{0xcf, 0x9a, 0x26, 0xcc, 0x1a, 0x37, 0x85, 0x06,
			0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88},
	}
	if err := b.AppendMessage(fb.ID, &want); err != nil {
		t.Fatalf("append: %v", err)
	}

	// The reader: another handle, taking the base and the log from disk.
	c := openIdx(dir, testUser)
	defer c.Close() //nolint:errcheck
	fc, err := c.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := c.GetMessages(fc.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var got *mailbox.MessageMeta
	for _, m := range msgs {
		if m.UID == 2 {
			got = m
		}
	}
	if got == nil {
		t.Fatalf("the second reader holds %d records and none is uid 2", len(msgs))
	}
	if got.VSize != want.VSize {
		t.Errorf("the second reader reports size %d, want %d -- %d is the record's map_uid",
			got.VSize, want.VSize, got.MapUID)
	}
	if got.MapUID != want.MapUID || got.SaveDate != want.SaveDate {
		t.Errorf("the second reader reads the storage key (%d, %d), want (%d, %d)",
			got.MapUID, got.SaveDate, want.MapUID, want.SaveDate)
	}
	if got.GUID != want.GUID {
		t.Errorf("the second reader reads guid %x, want %x", got.GUID, want.GUID)
	}
}

// The field widens every record, so a base written before it must take the new
// width too: a header left at the old one refuses every flush (#1709).
func TestAnOlderIndexTakesTheMdboxExtension(t *testing.T) {
	dir := t.TempDir()
	a := openIdx(dir, testUser)
	f, err := a.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	// The shape an older build left: records, no storage key among them.
	if err := a.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Size: 10}); err != nil {
		t.Fatal(err)
	}
	if err := a.withFolder(f.ID, func(fs *folderState) error { return fs.flush() }); err != nil {
		t.Fatal(err)
	}
	a.Close() //nolint:errcheck

	// The shape an older build left: the same base with no mdbox extension
	// declared, so its records are eight bytes narrower.
	before := stripExt(t, filepath.Join(testHome(dir, testUser), "yarilo.index"), extNameMdbox)

	b := openIdx(dir, testUser)
	defer b.Close() //nolint:errcheck
	fb, err := b.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.AppendMessage(fb.ID, &mailbox.MessageMeta{
		UID: 2, Size: 10, MapUID: 7, SaveDate: 1788000000,
	}); err != nil {
		t.Fatalf("the first record with a storage key: %v", err)
	}
	// The base itself, not the state in hand: a refused rewrite leaves the
	// folder serving from memory and the disk unchanged.
	if err := b.withFolder(fb.ID, func(fs *folderState) error { return fs.flush() }); err != nil {
		t.Fatalf("flush after the field was declared: %v", err)
	}
	b.Close() //nolint:errcheck

	path := filepath.Join(testHome(dir, testUser), "yarilo.index")
	onDisk, err := mailindex.Open(path)
	if err != nil {
		t.Fatalf("reopen the base: %v", err)
	}
	if got := onDisk.Header.RecordSize; got != before+mdboxRecSize {
		t.Errorf("the base says %d bytes a record, %d before the field", got, before)
	}

	c := openIdx(dir, testUser)
	defer c.Close() //nolint:errcheck
	fc, err := c.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := c.GetMessages(fc.ID, mailbox.SeqSet{})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("the folder holds %d records, want 2", len(msgs))
	}
	for _, m := range msgs {
		if m.UID == 2 && m.MapUID != 7 {
			t.Errorf("uid 2 says map uid %d, want 7", m.MapUID)
		}
		if m.Size != 10 {
			t.Errorf("uid %d reads back size %d, want 10", m.UID, m.Size)
		}
	}
}

// stripExt rewrites an index without the named extension, returning the record
// size that leaves: a base as an older build wrote it.
func stripExt(t *testing.T, path, name string) uint32 {
	t.Helper()
	f, err := mailindex.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	kept := make([]mailindex.Extension, 0, len(f.Extensions))
	for _, e := range f.Extensions {
		if e.Name != name {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(f.Extensions) {
		t.Fatalf("the fresh base declares no %q extension to strip", name)
	}
	layout, err := mailindex.ComputeRecordLayout(kept)
	if err != nil {
		t.Fatal(err)
	}
	extBytes, err := mailindex.EncodeExtHeaders(layout.Extensions)
	if err != nil {
		t.Fatal(err)
	}
	f.Extensions = layout.Extensions
	f.Layout = layout
	f.Header.RecordSize = layout.RecordSize
	f.Header.HeaderSize = uint32(mailindex.HeaderMinSize) + uint32(len(extBytes))
	for _, rec := range f.Records {
		delete(rec.Ext, name)
	}
	if _, err := mailindex.Recreate(f.ToRecreateInput(path)); err != nil {
		t.Fatal(err)
	}
	return layout.RecordSize
}
