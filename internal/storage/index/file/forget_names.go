package file

import (
	"os"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// ForgetStoredNames removes the sidecar without reading it, under the folder's
// exclusive lock. For a driver whose own store is the mapping there is nothing
// in it to take: keeping the file would leave a second answer on disk (#1700).
func (u *userIndex) ForgetStoredNames(folderID uint64) error {
	return u.withFolder(folderID, func(fs *folderState) error {
		if fs.namesFD != nil {
			_ = fs.namesFD.Close()
			fs.namesFD = nil
		}
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
