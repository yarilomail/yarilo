package file

import (
	"log/slog"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// RepairRecordTails rewrites the storage key, virtual size and guid of the
// named records from what the store says, and reports how many it changed.
//
// Every other size path stamps only what is missing; this one overwrites, so it
// names each record it touched and the number it replaced (#1770).
func (u *userIndex) RepairRecordTails(folderID uint64, tails map[uint32]mailbox.RecordTail) (int, error) {
	if len(tails) == 0 {
		return 0, nil
	}
	repaired := 0
	err := u.withFolder(folderID, func(fs *folderState) error {
		if err := fs.ensureVsizeExtLocked(); err != nil {
			return err
		}
		fs.ensureMdboxExtLocked()
		for _, rec := range fs.file.Records {
			tail, ok := tails[rec.UID]
			if !ok {
				continue
			}
			if rec.Ext == nil {
				rec.Ext = map[string][]byte{}
			}
			was := decodeVsizeRec(rec.Ext[extNameVsize])
			rec.Ext[extNameMdbox] = encodeMdboxRec(tail.MapUID, tail.SaveDate)
			rec.Ext[extNameVsize] = encodeVsizeRec(tail.VSize)
			rec.Ext[extNameGUID] = encodeGUIDRec(tail.GUID)
			repaired++
			slog.Info("fileindex: record tail rebuilt from the store",
				"user", fs.user, "folder", fs.folder, "uid", rec.UID,
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
