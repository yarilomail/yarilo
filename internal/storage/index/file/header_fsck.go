package file

import (
	"fmt"
	"log/slog"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
)

// reconcileHeaderLocked re-derives the header from the records it describes,
// as the reference's fsck does (#1831). Caller holds fs.mu.
func (fs *folderState) reconcileHeaderLocked() {
	var maxUID, seen, deleted uint32
	for i := range fs.recordCount() {
		maxUID = max(maxUID, fs.recUID(i))
		flags := fs.recFlags(i)
		if flags&mailindex.FlagSeen != 0 {
			seen++
		}
		if flags&mailindex.FlagDeleted != 0 {
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
		{"messages", &h.MessagesCount, uint32(fs.recordCount())},
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
	fs.headerFixed = true
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

// checkHeaderLocked is the cheap check after a load or a replay, as the
// reference's mail_index_map_check_header (INDEX-MEMORY.md §4): counts the
// header can be held to without reading the records, and the last uid from
// the last chunk only. A failed check is the one sign that recounts all.
func (fs *folderState) checkHeaderLocked() error {
	h := fs.file.Header
	n := uint32(fs.recordCount())
	ok := h.NextUID != 0 && h.MessagesCount == n &&
		h.SeenMessagesCount <= h.MessagesCount && h.DeletedMessagesCount <= h.MessagesCount
	if ok && n > 0 {
		if fs.recs != nil {
			if err := fs.recs.Load(int(n)-1, int(n)); err != nil {
				return fmt.Errorf("fileindex/header: %w", err)
			}
		}
		ok = fs.recUID(int(n)-1) < h.NextUID
	}
	if ok {
		return nil
	}
	if err := fs.loadRecords(); err != nil {
		return err
	}
	fs.reconcileHeaderLocked()
	return nil
}

// loadWriterRecords loads a writer's chunks, and on the load that brings in
// the last of them recounts the header on the way: a stale count does not
// outlive the first whole read, and no read is made for it alone.
func (fs *folderState) loadWriterRecords() error {
	if fs.recs == nil || fs.recs.AllLoaded() {
		return nil
	}
	if err := fs.loadRecords(); err != nil {
		return err
	}
	fs.reconcileHeaderLocked()
	return nil
}
