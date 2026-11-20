package file

import (
	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// StampSizes writes a virtual size into records carrying none and refreshes the
// quota aggregate: a recovered folder counted as empty otherwise (#1728).
func (u *userIndex) StampSizes(folderID uint64, vsizes map[uint32]uint32) (int, error) {
	stamped := 0
	err := u.withFolderSite(folderID, lockSiteStampSizes, func(fs *folderState) error {
		if err := fs.ensureVsizeExtLocked(); err != nil {
			return err
		}
		var upds []mailindex.TxExtRecUpdate
		for seq := range fs.recordCount() {
			vsize, known := vsizes[fs.recUID(seq)]
			if !known || vsize == 0 {
				continue
			}
			if have, _ := fs.recExt(seq, extNameVsize); decodeVsizeRec(have) != 0 {
				continue
			}
			upds = append(upds, mailindex.TxExtRecUpdate{UID: fs.recUID(seq), Data: encodeVsizeRec(vsize)})
		}
		if len(upds) == 0 {
			return nil
		}
		stamped = len(upds)
		recs, err := fs.extRecRecords(extNameVsize, upds)
		if err != nil {
			return err
		}
		if err := fs.applyRecords(recs); err != nil {
			return err
		}
		// The folder's aggregate moves with the sizes, journalled as its
		// header extension's update.
		hadAggregate := findExt(fs.file.Extensions, extNameHdrVsize) != nil
		fs.recalcVsizeLocked()
		fs.persistVsizeLocked()
		if !hadAggregate {
			// A base from before the aggregate gains its header here, which no
			// journal record introduces: the rare rewrite.
			if err := fs.appendMutLog(recs...); err != nil {
				return err
			}
			return fs.flush()
		}
		intro, err := fs.extIntroRecord(extNameHdrVsize)
		if err != nil {
			return err
		}
		agg := [][]byte{intro, encLogRec(mailindex.TxTypeExtHdrUpdate, 0,
			mailindex.EncodeTxExtHdrUpdatePayload(mailindex.TxExtHdrUpdate{Offset: 0, Data: encodeHdrVsize(fs.vsize)}))}
		return fs.appendMutLog(append(recs, agg...)...)
	})
	return stamped, err
}

var _ mailbox.SizeStamper = (*userIndex)(nil)
