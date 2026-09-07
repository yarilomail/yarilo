package file

import (
	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// SetFlagsDirty marks, or clears, a record whose flags have not reached storage:
// until they do, what the file name says about them is older than what the
// client was told, and a sync must not take it (#1700).
func (u *userIndex) SetFlagsDirty(folderID uint64, uid uint32, dirty bool) error {
	return u.withFolder(folderID, func(fs *folderState) error {
		for _, rec := range fs.file.Records {
			if rec.UID != uid {
				continue
			}
			before := rec.Flags
			if dirty {
				rec.Flags |= mailindex.FlagDirty
				fs.file.Header.Flags |= mailindex.HdrFlagHaveDirty
			} else {
				rec.Flags &^= mailindex.FlagDirty
			}
			if rec.Flags == before {
				return nil
			}
			return fs.flush(true)
		}
		return nil
	})
}

var _ mailbox.FlagsDirtyMarker = (*userIndex)(nil)
