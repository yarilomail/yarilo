package file

import (
	"os"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// ForgetStoredNames removes the sidecar unread, under the folder lock: where the
// driver's own store is the mapping, the file is a second answer (#1700).
func (u *userIndex) ForgetStoredNames(folderID uint64) error {
	return u.withFolder(folderID, func(fs *folderState) error {
		if err := os.Remove(namesPath(fs.indexDir)); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	})
}

// IndexDirFor is where this folder's index lives, so a driver can ask whether a
// sidecar is there without a second spelling of the path rule.
func (u *userIndex) IndexDirFor(folder string) string { return u.indexDir(folder) }

var _ mailbox.StoredNameForgetter = (*userIndex)(nil)
