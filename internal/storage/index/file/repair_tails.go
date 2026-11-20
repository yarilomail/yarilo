package file

import (
	"log/slog"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// RepairRecordTails rewrites the storage key, virtual size and guid of the named
// records. It overwrites where every other path stamps, so it names each one.
func (u *userIndex) RepairRecordTails(folderID uint64, tails map[uint32]mailbox.RecordTail) (int, error) {
	if len(tails) == 0 {
		return 0, nil
	}
	repaired := 0
	err := u.withFolderSite(folderID, lockSiteRepairTails, func(fs *folderState) error {
		if err := fs.ensureVsizeExtLocked(); err != nil {
			return err
		}
		fs.ensureMdboxExtLocked()
		// A base predating the field would drop the bytes at the flush.
		if err := fs.declareRecordExtLocked(extNameGUID, encodeGUIDHdr(guidStatePending),
			guidRecSize, 1, fs.file.Header.UIDValidity); err != nil {
			return err
		}
		// A repair after damage rewrites the base, not the journal: the
		// writer's own chunks are changed and the folder is folded.
		for seq := range fs.recordCount() {
			uid := fs.recUID(seq)
			tail, ok := tails[uid]
			if !ok {
				continue
			}
			v, _ := fs.recExt(seq, extNameVsize)
			was := decodeVsizeRec(v)
			fs.recSetExt(seq, extNameMdbox, encodeMdboxRec(tail.MapUID, tail.SaveDate))
			fs.recSetExt(seq, extNameVsize, encodeVsizeRec(tail.VSize))
			fs.recSetExt(seq, extNameGUID, encodeGUIDRec(tail.GUID))
			repaired++
			slog.Info("fileindex: record tail rebuilt from the store",
				"user", fs.user, "folder", fs.folder, "uid", uid,
				"map_uid", tail.MapUID, "size_was", was, "size_now", tail.VSize)
		}
		if repaired == 0 {
			return nil
		}
		fs.recalcVsizeLocked()
		fs.persistVsizeLocked()
		return fs.flush()
	})
	return repaired, err
}

var _ mailbox.TailRepairer = (*userIndex)(nil)
