package file

import "github.com/yarilomail/yarilo/v2/pkg/mailbox"

// SizelessUIDs are the records carrying no virtual size. In memory, so the
// driver can ask on every open and fill them from storage (#1728).
func (u *userIndex) SizelessUIDs(folderID uint64) ([]uint32, error) {
	var out []uint32
	err := u.withFolderROUnlocked(folderID, func(fs *folderState) error {
		out = out[:0] // a read the base moved under runs again
		if err := fs.loadRecords(); err != nil {
			return err
		}
		for i := range fs.recordCount() {
			if v, _ := fs.recExt(i, extNameVsize); decodeVsizeRec(v) == 0 {
				out = append(out, fs.recUID(i))
			}
		}
		return nil
	})
	return out, err
}

var _ mailbox.SizelessLister = (*userIndex)(nil)
