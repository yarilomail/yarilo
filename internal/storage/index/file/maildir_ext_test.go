package file

import (
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The extension is registered the way the reference registers it: a 36-byte
// header, no records, no alignment (maildir-storage.c:318-319).
func TestTheMaildirExtensionIsRegisteredLikeTheReference(t *testing.T) {
	// Through the writer, not through a call this row makes itself: the shape
	// under test is the one the driver's stamp leaves behind.
	ui := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: t.TempDir()}).(*userHandle).ui
	f, err := ui.OpenFolder("INBOX", 7, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ui.SetMaildirStamp(f.ID, mailbox.MaildirStamp{UIDListSize: 7, UIDListMtime: 8}); err != nil {
		t.Fatal(err)
	}

	var ext *mailindex.Extension
	if err := ui.withFolderROUnlocked(f.ID, func(fs *folderState) error {
		ext = findExt(fs.file.Extensions, extNameMaildir)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if ext == nil {
		t.Fatal("the stamp left no maildir extension in the index")
	}
	if ext.HdrSize != maildirHdrSize || ext.RecordSize != 0 || ext.RecordAlign != 0 {
		t.Errorf("registered (%d, %d, %d), want (36, 0, 0)",
			ext.HdrSize, ext.RecordSize, ext.RecordAlign)
	}
}

// And a header written in that shape by something else reads back as a stamp:
// the fields are theirs, in their order.
func TestAStampWrittenInTheReferenceShapeReadsBack(t *testing.T) {
	want := mailbox.MaildirStamp{
		NewCheckTime: 1, NewMtime: 2, NewMtimeNsecs: 3,
		CurCheckTime: 4, CurMtime: 5, CurMtimeNsecs: 6,
		UIDListMtime: 7, UIDListMtimeNsecs: 8, UIDListSize: 9,
	}
	raw := encodeMaildirHdr(want)
	if len(raw) != maildirHdrSize {
		t.Fatalf("the header is %d bytes, theirs is %d", len(raw), maildirHdrSize)
	}
	// Byte 24 is where uidlist_mtime starts in their struct: six uint32 of
	// directory bookkeeping come first.
	if raw[24] != 7 {
		t.Errorf("uidlist_mtime is not the seventh field: byte 24 = %d", raw[24])
	}
	got, ok := decodeMaildirHdr(raw)
	if !ok || got != want {
		t.Errorf("read back %+v, wrote %+v", got, want)
	}
	if _, ok := decodeMaildirHdr(raw[:maildirHdrSize-1]); ok {
		t.Error("a short header read as a stamp, so a truncated index answers with numbers")
	}
}
