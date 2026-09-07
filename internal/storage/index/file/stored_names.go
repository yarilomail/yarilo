package file

import "github.com/yarilomail/yarilo/pkg/mailbox"

// StoredNames reads what an older build's sidecar named, so a driver whose own
// store is the mapping can take the names into it before the file goes (#1726).
func (u *userIndex) StoredNames(folderID uint64) (map[uint32]string, error) {
	var out map[uint32]string
	err := u.withFolderRO(folderID, func(fs *folderState) error {
		names, lerr := loadNames(fs.indexDir)
		out = names
		return lerr
	})
	return out, err
}

var _ mailbox.StoredNameLister = (*userIndex)(nil)
