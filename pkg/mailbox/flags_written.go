package mailbox

import "log/slog"

// FlagsWritten records settled flags where the driver keeps them, marking a
// record dirty when its write does not land. Best effort (#1724).
func FlagsWritten(idx UserIndex, box UserMailbox, folderID uint64, folder string, writes []FlagWrite) []FlagWriteResult {
	named := make([]FlagWrite, 0, len(writes))
	for _, w := range writes {
		if w.Filename != "" {
			named = append(named, w)
		}
	}
	if len(named) == 0 {
		return nil
	}
	driver := Driver(box)
	var results []FlagWriteResult
	switch w := driver.(type) {
	case FlagWriterMulti:
		// The batch form takes the folder lock once, not once per message (#1623).
		results = w.WriteFlagsMulti(folder, named)
	case FlagWriter:
		results = make([]FlagWriteResult, len(named))
		for i, fw := range named {
			name, err := w.WriteFlags(folder, fw.Filename, fw.Flags, fw.Keywords)
			results[i] = FlagWriteResult{UID: fw.UID, Filename: name, Err: err}
		}
	default:
		return nil
	}
	dirt, marks := idx.(FlagsDirtyMarker)
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
