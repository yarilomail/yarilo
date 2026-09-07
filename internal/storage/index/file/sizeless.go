package file

import "github.com/yarilomail/yarilo/pkg/mailbox"

// SizelessUIDs are the records carrying no virtual size. In memory, so the
// driver can ask on every open and fill them from storage (#1728).
func (u *userIndex) SizelessUIDs(folderID uint64) ([]uint32, error) {
	var out []uint32
	err := u.withFolderRO(folderID, func(fs *folderState) error {
		for _, rec := range fs.file.Records {
			if decodeVsizeRec(rec.Ext[extNameVsize]) == 0 {
				out = append(out, rec.UID)
			}
		}
		return nil
	})
	return out, err
}

var _ mailbox.SizelessLister = (*userIndex)(nil)
