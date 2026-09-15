package file

import (
	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
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
	{
		for _, rec := range fs.file.Records {
			if rec.UID != uid {
				continue
			}
			before := rec.Flags
			if dirty {
				rec.Flags |= mailindex.FlagDirty
			} else {
				rec.Flags &^= mailindex.FlagDirty
			}
			if rec.Flags == before {
				return nil, nil
			}
			recs := [][]byte{
				encLogRec(mailindex.TxTypeFlagUpdate, 0, mailindex.EncodeTxFlagUpdatePayload([]mailindex.TxFlagUpdate{{
					UID1: uid, UID2: uid, AddFlags: rec.Flags, RemoveFlags: ^rec.Flags,
				}})),
			}
			if dirty && fs.file.Header.Flags&mailindex.HdrFlagHaveDirty == 0 {
				fs.file.Header.Flags |= mailindex.HdrFlagHaveDirty
				recs = append(recs, encU32Update(20, uint32(fs.file.Header.Flags)))
			}
			return recs, nil
		}
		return nil, nil
	}
}

var _ mailbox.FlagsDirtyMarker = (*userIndex)(nil)
