package idxrebuild

import (
	"fmt"
	"strconv"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// ShiftedTail is a record whose tail was written at a width its folder's base
// did not announce, so every field from the storage key on reads eight bytes
// early: the virtual size comes back as the record's own map_uid and the guid
// carries the halves of two other fields (#1770).
//
// Both halves of the mark are needed. A save stamps a save_date, so a zero one
// says the field was never written as itself; alone it would also match a
// record from a build that predates the field.
func ShiftedTail(m *mailbox.MessageMeta) bool {
	return m != nil && m.MapUID != 0 && m.SaveDate == 0 && m.VSize == m.MapUID
}

// TailStats is what one repair pass touched.
type TailStats struct {
	Checked  int
	Shifted  int
	Repaired int
}

// StoredTails keys a scan of the store by the storage key each message is named
// by, so a repair reads the truth once per account rather than once per record.
func StoredTails(b mailbox.Box) (map[uint32]mailbox.ScanRecord, error) {
	// The empty folder name is only legal for a store whose scan is
	// storage-wide; a per-folder driver has no storage key to key this by.
	agnostic, ok := mailbox.Driver(b.Store()).(mailbox.FolderAgnosticStorage)
	if !ok || !agnostic.FolderAgnosticScan() {
		return nil, nil
	}
	scanned, err := b.Store().Scan("")
	if err != nil {
		return nil, fmt.Errorf("idxrebuild/tails: scan: %w", err)
	}
	out := make(map[uint32]mailbox.ScanRecord, len(scanned))
	for _, rec := range scanned {
		key, perr := strconv.ParseUint(rec.Filename, 10, 32)
		if perr != nil {
			continue
		}
		out[uint32(key)] = rec
	}
	return out, nil
}

// RepairShiftedTails rebuilds every shifted record's tail in one folder from
// what the store holds, and reports what it checked and changed.
//
// The message carries no save_date of its own, so the repair takes the receive
// stamp its trailer does carry; a record whose message names no date keeps the
// index's own, and one with neither is left for an operator to look at rather
// than stamped with a number nothing on disk supports.
func RepairShiftedTails(b mailbox.Box, folder *mailbox.Folder, stored map[uint32]mailbox.ScanRecord) (TailStats, error) {
	var stats TailStats
	repairer, ok := b.Index().(mailbox.TailRepairer)
	if !ok {
		return stats, nil
	}
	msgs, err := b.Index().GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		return stats, fmt.Errorf("idxrebuild/tails: %q: %w", folder.Name, err)
	}
	stats.Checked = len(msgs)
	tails := map[uint32]mailbox.RecordTail{}
	for _, m := range msgs {
		if !ShiftedTail(m) {
			continue
		}
		stats.Shifted++
		rec, known := stored[m.MapUID]
		if !known || rec.VSize == 0 {
			continue
		}
		saved := rec.InternalDate
		if saved.IsZero() {
			saved = m.InternalDate
		}
		if saved.IsZero() {
			continue
		}
		tails[m.UID] = mailbox.RecordTail{
			MapUID:   m.MapUID,
			SaveDate: uint32(saved.Unix()),
			VSize:    rec.VSize,
			GUID:     rec.GUID,
		}
	}
	if len(tails) == 0 {
		return stats, nil
	}
	n, err := repairer.RepairRecordTails(folder.ID, tails)
	if err != nil {
		return stats, fmt.Errorf("idxrebuild/tails: %q: %w", folder.Name, err)
	}
	stats.Repaired = n
	return stats, nil
}
