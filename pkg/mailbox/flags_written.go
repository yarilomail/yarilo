package mailbox

import "log/slog"

// FlagsWritten records settled flags where the driver keeps them and marks each
// record dirty when its write does not land: on a driver whose name carries the
// flags, a record the store never heard of is reverted by the next sync (#1724).
//
// Best effort by design: the flags are already committed to the index, which is
// what the client was told. A rename that fails is a warning and a later
// reconcile, not an error on a write that succeeded.
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
