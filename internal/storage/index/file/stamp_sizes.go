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
		// The folder's aggregate moves with the sizes, journalled as its header
		// extension's update; a base from before the aggregate gains the
		// extension the same way, by an intro (INDEX-MEMORY.md §4).
		fs.recalcVsizeLocked()
		var intro []byte
		if findExt(fs.file.Extensions, extNameHdrVsize) == nil {
			intro = encLogRec(mailindex.TxTypeExtIntro, 0, mailindex.EncodeTxExtIntroPayload(mailindex.TxExtIntro{
				ExtID: mailindex.TxExtIntroNewExt, ResetID: fs.file.Header.UIDValidity, HdrSize: hdrVsizeSize,
				RecordAlign: 8, Name: extNameHdrVsize,
			}))
		} else if intro, err = fs.extIntroRecord(extNameHdrVsize); err != nil {
			return err
		}
		agg := [][]byte{intro, encLogRec(mailindex.TxTypeExtHdrUpdate, 0,
			mailindex.EncodeTxExtHdrUpdatePayload(mailindex.TxExtHdrUpdate{Offset: 0, Data: encodeHdrVsize(fs.vsize)}))}
		if err := fs.applyRecords(agg); err != nil {
			return err
		}
		return fs.appendMutLog(append(recs, agg...)...)
	})
	return stamped, err
}

var _ mailbox.SizeStamper = (*userIndex)(nil)
