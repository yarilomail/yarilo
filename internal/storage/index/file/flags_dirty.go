package file

import (
	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// SetFlagsDirty marks a record whose flags have not reached storage (#1700).
// Journalled: a mark in the base alone is erased by the log replayed over it.
func (u *userIndex) SetFlagsDirty(folderID uint64, uid uint32, dirty bool) error {
	return u.withFolderSite(folderID, lockSiteFlagsDirty, func(fs *folderState) error {
		recs, err := fs.markDirtyLocked(uid, dirty)
		if err != nil || len(recs) == 0 {
			return err
		}
		return fs.appendMutLog(recs...)
	})
}

// markDirtyLocked is the in-memory half, returning the log records it needs so
// a transaction can carry a command's worth (#1809).
func (fs *folderState) markDirtyLocked(uid uint32, dirty bool) ([][]byte, error) {
	seq, ok := fs.recSeqOfUID(uid)
	if !ok {
		return nil, nil
	}
	before := fs.recFlags(seq)
	now := before &^ mailindex.FlagDirty
	if dirty {
		now = before | mailindex.FlagDirty
	}
	if now == before {
		return nil, nil
	}
	recs := [][]byte{
		encLogRec(mailindex.TxTypeFlagUpdate, 0, mailindex.EncodeTxFlagUpdatePayload([]mailindex.TxFlagUpdate{{
			UID1: uid, UID2: uid, AddFlags: now, RemoveFlags: ^now,
		}})),
	}
	if dirty && fs.file.Header.Flags&mailindex.HdrFlagHaveDirty == 0 {
		recs = append(recs, encU32Update(20, uint32(fs.file.Header.Flags|mailindex.HdrFlagHaveDirty)))
	}
	if err := fs.applyRecords(recs); err != nil {
		return nil, err
	}
	return recs, nil
}

var _ mailbox.FlagsDirtyMarker = (*userIndex)(nil)
