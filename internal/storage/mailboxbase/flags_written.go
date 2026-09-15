package mailboxbase

import (
	"log/slog"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// FlagsWritten records settled flags where the driver keeps them, marking a
// record dirty when its write does not land. Best effort (#1724).
func FlagsWritten(idx mailbox.UserIndex, box mailbox.UserMailbox, folderID uint64, folder string, writes []mailbox.FlagWrite) []mailbox.FlagWriteResult {
	named := make([]mailbox.FlagWrite, 0, len(writes))
	for _, w := range writes {
		if w.Filename != "" {
			named = append(named, w)
		}
	}
	if len(named) == 0 {
		return nil
	}
	driver := mailbox.Driver(box)
	var results []mailbox.FlagWriteResult
	switch w := driver.(type) {
	case mailbox.FlagWriterMulti:
		// The batch form takes the folder lock once, not once per message (#1623).
		results = w.WriteFlagsMulti(folder, named)
	case mailbox.FlagWriter:
		results = make([]mailbox.FlagWriteResult, len(named))
		for i, fw := range named {
			name, err := w.WriteFlags(folder, fw.Filename, fw.Flags, fw.Keywords)
			results[i] = mailbox.FlagWriteResult{UID: fw.UID, Filename: name, Err: err}
		}
	default:
		return nil
	}
	// One transaction for the command's marks: a call per message took the
	// index lock per message, which was 1633 of 3318 acquisitions (#1809).
	tx, terr := idx.Begin(folderID)
	if terr != nil {
		slog.Warn("mailbox: could not open the index to record which flags landed",
			"folder", folder, "err", terr)
	}
	for _, res := range results {
		if res.Err != nil {
			slog.Warn("mailbox: could not record flags in storage",
				"folder", folder, "uid", res.UID, "err", res.Err)
		}
		if terr == nil {
			tx.MarkDirty(res.UID, res.Err != nil)
		}
	}
	if terr == nil {
		defer tx.Rollback()
		if _, cerr := tx.Commit(); cerr != nil {
			slog.Warn("mailbox: could not record which flags landed",
				"folder", folder, "err", cerr)
		}
	}
	return results
}
