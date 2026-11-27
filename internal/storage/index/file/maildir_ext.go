package file

import (
	"encoding/binary"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// The maildir extension: what the directories and uid list looked like when
// this index was written (nine uint32 little endian, as the reference does).
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

// MaildirStamp reads the stamp this index was written with; absent reads as
// "this index does not say", which sends the caller to the directory.
func (u *userIndex) MaildirStamp(folderID uint64) (mailbox.MaildirStamp, bool) {
	var (
		out mailbox.MaildirStamp
		ok  bool
	)
	err := u.withFolderHeaderUnlocked(folderID, func(fs *folderState) error {
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
		return fs.setMaildirStampLocked(s)
	})
}

// setMaildirStampLocked records the stamp under the caller's hold: only on a
// difference, journalled as the extension's header update, as the reference
// writes it, so a delivery stamps the list it appended to without a base
// rewrite (#2643).
func (fs *folderState) setMaildirStampLocked(s mailbox.MaildirStamp) error {
	recs, registered, err := fs.maildirStampRecords(s)
	if err != nil || registered {
		if err == nil {
			err = fs.journalRecords(recs)
		}
		return err
	}
	return fs.addMaildirStampLocked(s)
}

// maildirStampRecords is the journal of moving the stamp to s: none when it is
// s already. registered false says the extension is not there yet.
func (fs *folderState) maildirStampRecords(s mailbox.MaildirStamp) (recs [][]byte, registered bool, err error) {
	ext := findExt(fs.file.Extensions, extNameMaildir)
	if ext == nil {
		return nil, false, nil
	}
	if was, ok := decodeMaildirHdr(ext.HdrData); ok && was == s {
		metricStampUnchanged.Inc()
		return nil, true, nil
	}
	intro, err := fs.extIntroRecord(extNameMaildir)
	if err != nil {
		return nil, true, err
	}
	return [][]byte{intro, encLogRec(mailindex.TxTypeExtHdrUpdate, 0,
		mailindex.EncodeTxExtHdrUpdatePayload(mailindex.TxExtHdrUpdate{Offset: 0, Data: encodeMaildirHdr(s)}))}, true, nil
}

// addMaildirStampLocked registers the extension with s, writing the base.
func (fs *folderState) addMaildirStampLocked(s mailbox.MaildirStamp) error {
	data := encodeMaildirHdr(s)
	// Registered (36, 0, 0) as the reference does: a header-only extension has
	// no records to align.
	if err := fs.file.AddHeaderExtension(extNameMaildir, data, 0, fs.file.Header.UIDValidity); err != nil {
		return fmt.Errorf("fileindex/maildir-stamp: %w", err)
	}
	return fs.flush()
}

// StampUnchangedCount exposes the counter to a driver's row: the number that
// says a pass changing nothing wrote nothing.
func StampUnchangedCount() prometheus.Counter { return metricStampUnchanged }

// keepDirsOf is s with the recorded directory times when s carries the list
// alone: a save stamps the list it wrote, not the walk it did not do (#2661).
func (fs *folderState) keepDirsOf(s mailbox.MaildirStamp) mailbox.MaildirStamp {
	if s.HasDirs() {
		return s
	}
	if was, ok := fs.maildirStampLocked(); ok {
		s.NewCheckTime, s.NewMtime, s.NewMtimeNsecs = was.NewCheckTime, was.NewMtime, was.NewMtimeNsecs
		s.CurCheckTime, s.CurMtime, s.CurMtimeNsecs = was.CurCheckTime, was.CurMtime, was.CurMtimeNsecs
	}
	return s
}

func (fs *folderState) maildirStampLocked() (mailbox.MaildirStamp, bool) {
	if ext := findExt(fs.file.Extensions, extNameMaildir); ext != nil {
		return decodeMaildirHdr(ext.HdrData)
	}
	return mailbox.MaildirStamp{}, false
}

// CarryDirTimes moves the recorded time of each directory our write changed
// from pre to post, when it still reads pre to the nanosecond; another time
// there is someone else's change, and the stamp stays for a walk to settle
// (#2661). As the reference, the check time is not moved back.
func (u *userIndex) CarryDirTimes(folderID uint64, pre, post mailbox.DirTimes) error {
	return u.withFolderSite(folderID, lockSiteMaildirStamp, func(fs *folderState) error {
		return fs.carryDirTimesLocked(pre, post)
	})
}

func (fs *folderState) carryDirTimesLocked(pre, post mailbox.DirTimes) error {
	was, ok := fs.maildirStampLocked()
	if !ok || !was.HasDirs() {
		return nil // never walked into the stamp: the first walk records it
	}
	s := carriedStamp(was, pre, post)
	if s == was {
		return nil
	}
	return fs.setMaildirStampLocked(s)
}

// carriedStamp is was with each changed directory carried from pre to post.
func carriedStamp(was mailbox.MaildirStamp, pre, post mailbox.DirTimes) mailbox.MaildirStamp {
	s := was
	if (pre.NewSec != post.NewSec || pre.NewNsec != post.NewNsec) && was.NewMtime == pre.NewSec && was.NewMtimeNsecs == pre.NewNsec {
		s.NewMtime, s.NewMtimeNsecs = post.NewSec, post.NewNsec
		s.NewCheckTime = max(s.NewCheckTime, post.NewSec)
	}
	if (pre.CurSec != post.CurSec || pre.CurNsec != post.CurNsec) && was.CurMtime == pre.CurSec && was.CurMtimeNsecs == pre.CurNsec {
		s.CurMtime, s.CurMtimeNsecs = post.CurSec, post.CurNsec
		s.CurCheckTime = max(s.CurCheckTime, post.CurSec)
	}
	return s
}
