package file

import (
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// StampSizes writes a virtual size into records carrying none and refreshes the
// quota aggregate: a recovered folder counted as empty otherwise (#1728).
func (u *userIndex) StampSizes(folderID uint64, vsizes map[uint32]uint32) (int, error) {
	stamped := 0
	err := u.withFolder(folderID, func(fs *folderState) error {
		if err := fs.ensureVsizeExtLocked(); err != nil {
			return err
		}
		for _, rec := range fs.file.Records {
			vsize, known := vsizes[rec.UID]
			if !known || vsize == 0 {
				continue
			}
			if decodeVsizeRec(rec.Ext[extNameVsize]) != 0 {
				continue
			}
			if rec.Ext == nil {
				rec.Ext = map[string][]byte{}
			}
			rec.Ext[extNameVsize] = encodeVsizeRec(vsize)
			stamped++
		}
		if stamped == 0 {
			return nil
		}
		fs.recalcVsizeLocked()
		fs.persistVsizeLocked()
		return fs.flush()
	})
	return stamped, err
}

var _ mailbox.SizeStamper = (*userIndex)(nil)
