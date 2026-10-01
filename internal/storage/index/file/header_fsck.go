package file

import (
	"log/slog"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
)

// reconcileHeaderLocked re-derives the header from the records it describes,
// as the reference's fsck does (mail-index-fsck.c:327-410). Caller holds fs.mu.
func (fs *folderState) reconcileHeaderLocked() {
	var maxUID, seen, deleted uint32
	for _, rec := range fs.file.Records {
		if rec.UID > maxUID {
			maxUID = rec.UID
		}
		if rec.Flags&mailindex.FlagSeen != 0 {
			seen++
		}
		if rec.Flags&mailindex.FlagDeleted != 0 {
			deleted++
		}
	}
	h := &fs.file.Header
	if h.NextUID <= maxUID {
		fs.headerCorrected("next_uid", h.NextUID, maxUID+1)
		h.NextUID = maxUID + 1
	}
	for _, c := range []struct {
		field string
		have  *uint32
		want  uint32
	}{
		{"messages", &h.MessagesCount, uint32(len(fs.file.Records))},
		{"seen", &h.SeenMessagesCount, seen},
		{"deleted", &h.DeletedMessagesCount, deleted},
	} {
		if *c.have != c.want {
			fs.headerCorrected(c.field, *c.have, c.want)
			*c.have = c.want
		}
	}
}

func (fs *folderState) headerCorrected(field string, was, now uint32) {
	metricHeaderCorrected.WithLabelValues(field).Inc()
	slog.Warn("fileindex: header corrected from its records",
		"trace_id", fs.traceID, "folder", fs.folder, "field", field, "was", was, "now", now)
}

// moveCount adjusts a flag count for one record whose flags went from old to now.
func moveCount(n uint32, old, now, flag mailindex.MailFlag) uint32 {
	switch {
	case old&flag == 0 && now&flag != 0:
		return n + 1
	case old&flag != 0 && now&flag == 0 && n > 0:
		return n - 1
	}
	return n
}
