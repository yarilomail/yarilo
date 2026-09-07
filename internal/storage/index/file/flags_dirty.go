package file

import (
	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// SetFlagsDirty marks a record whose flags have not reached storage: until they
// do, the name says something older than the client was told (#1700).
//
// Journalled like any other flag change: a mark written to the base alone is
// erased by the log the next reader replays over it (#1724).
func (u *userIndex) SetFlagsDirty(folderID uint64, uid uint32, dirty bool) error {
	return u.withFolder(folderID, func(fs *folderState) error {
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
				return nil
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
			return fs.appendMutLog(recs...)
		}
		return nil
	})
}

var _ mailbox.FlagsDirtyMarker = (*userIndex)(nil)
