package file

import "github.com/yarilomail/yarilo/pkg/mailbox"

// RefreshFolder re-reads the base and the log under the folder's lock, so a
// caller deciding what a folder holds decides on what is there and not on what
// this process last read (#1739).
func (u *userIndex) RefreshFolder(folderID uint64) error {
	return u.withFolder(folderID, func(fs *folderState) error { return fs.reloadNow() })
}

var _ mailbox.FolderRefresher = (*userIndex)(nil)
