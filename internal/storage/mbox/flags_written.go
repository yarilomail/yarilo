package mbox

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
	dirt, marks := idx.(mailbox.FlagsDirtyMarker)
	for _, res := range results {
		if res.Err != nil {
			slog.Warn("mailbox: could not record flags in storage",
				"folder", folder, "uid", res.UID, "err", res.Err)
		}
		if marks {
			_ = dirt.SetFlagsDirty(folderID, res.UID, res.Err != nil)
		}
	}
	return results
}
