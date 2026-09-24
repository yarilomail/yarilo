package file

import (
	"encoding/binary"
	"fmt"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The maildir extension: what a folder's directories and uid list looked like
// when this index was written, in the reference's layout
// (maildir-storage.h:52-56) -- nine uint32, little endian.
const (
	extNameMaildir = "maildir"
	maildirHdrSize = 36
)

// The stamp itself is mailbox.MaildirStamp: the driver writes it and the
// reference reads it, so it belongs to neither package alone.
func encodeMaildirHdr(s mailbox.MaildirStamp) []byte {
	b := make([]byte, maildirHdrSize)
	for i, v := range [9]uint32{
		s.NewCheckTime, s.NewMtime, s.NewMtimeNsecs,
		s.CurCheckTime, s.CurMtime, s.CurMtimeNsecs,
		s.UIDListMtime, s.UIDListMtimeNsecs, s.UIDListSize,
	} {
		binary.LittleEndian.PutUint32(b[i*4:], v)
	}
	return b
}

func decodeMaildirHdr(b []byte) (mailbox.MaildirStamp, bool) {
	if len(b) < maildirHdrSize {
		return mailbox.MaildirStamp{}, false
	}
	v := func(i int) uint32 { return binary.LittleEndian.Uint32(b[i*4:]) }
	return mailbox.MaildirStamp{
		NewCheckTime: v(0), NewMtime: v(1), NewMtimeNsecs: v(2),
		CurCheckTime: v(3), CurMtime: v(4), CurMtimeNsecs: v(5),
		UIDListMtime: v(6), UIDListMtimeNsecs: v(7), UIDListSize: v(8),
	}, true
}

// MaildirStamp reads the stamp this index was written with. Absent reads as
// "this index does not say", which sends the caller to the directory.
func (u *userIndex) MaildirStamp(folderID uint64) (mailbox.MaildirStamp, bool) {
	var (
		out mailbox.MaildirStamp
		ok  bool
	)
	err := u.withFolderROUnlocked(folderID, func(fs *folderState) error {
		if ext := findExt(fs.file.Extensions, extNameMaildir); ext != nil {
			out, ok = decodeMaildirHdr(ext.HdrData)
		}
		return nil
	})
	if err != nil {
		return mailbox.MaildirStamp{}, false
	}
	return out, ok
}

// SetMaildirStamp records it, in the reference's layout so that install reads
// this index as its own.
func (u *userIndex) SetMaildirStamp(folderID uint64, s mailbox.MaildirStamp) error {
	return u.withFolderSite(folderID, lockSiteMaildirStamp, func(fs *folderState) error {
		data := encodeMaildirHdr(s)
		if ext := findExt(fs.file.Extensions, extNameMaildir); ext != nil {
			ext.HdrData, ext.HdrSize = data, uint32(len(data))
			return fs.flush()
		}
		// AddHeaderExtension also fixes Header.HeaderSize, which Recreate
		// rejects on mismatch.
		if err := fs.file.AddHeaderExtension(extNameMaildir, data, 4, fs.file.Header.UIDValidity); err != nil {
			return fmt.Errorf("fileindex/maildir-stamp: %w", err)
		}
		return fs.flush()
	})
}
