package idxrebuild

import (
	"fmt"
	"log/slog"
	"strconv"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// ShiftedTail marks a record read eight bytes early, from the storage key on.
// Both halves: a zero save_date alone predates the field just as well (#1770).
func ShiftedTail(m *mailbox.MessageMeta) bool {
	return m != nil && m.MapUID != 0 && m.SaveDate == 0 && m.VSize == m.MapUID
}

// TailStats is what one repair pass touched. Skipped is a shifted record the
// store could not answer for.
type TailStats struct {
	Checked  int
	Shifted  int
	Repaired int
	Skipped  int
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
// what the store holds, and reports what it checked, changed and left.
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
		tail, why := tailFromStore(m, stored)
		if why != "" {
			stats.Skipped++
			slog.Warn("idxrebuild: shifted record left as it is",
				"user", b.Username(), "folder", folder.Name, "uid", m.UID,
				"map_uid", m.MapUID, "reason", why)
			continue
		}
		tails[m.UID] = tail
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

// tailFromStore builds one record's replacement tail, or names why it cannot.
// The date is the trailer's own stamp: an invented one just hides the record.
func tailFromStore(m *mailbox.MessageMeta, stored map[uint32]mailbox.ScanRecord) (mailbox.RecordTail, string) {
	rec, known := stored[m.MapUID]
	if !known {
		return mailbox.RecordTail{}, "the store holds no message under this key"
	}
	if rec.VSize == 0 {
		return mailbox.RecordTail{}, "the message reports no virtual size"
	}
	saved := rec.InternalDate
	if saved.IsZero() {
		saved = m.InternalDate
	}
	if saved.IsZero() {
		return mailbox.RecordTail{}, "neither the message nor the record carries a date"
	}
	return mailbox.RecordTail{
		MapUID:   m.MapUID,
		SaveDate: uint32(saved.Unix()),
		VSize:    rec.VSize,
		GUID:     rec.GUID,
	}, ""
}
