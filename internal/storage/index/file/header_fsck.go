package file

import (
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

// checkHeaderLocked is the cheap check after a load or a replay
// (INDEX-MEMORY.md §4): counts the header can be held to without reading the
// records, and the last uid from the last chunk only. A failed check is the
// one sign that recounts all.
func (fs *folderState) checkHeaderLocked() error {
	if len(fs.pending) > 0 {
		// Counted from the journal's absolute updates, or unknown and
		// resolved by a load at the end of the replay.
		return nil
	}
	h := fs.file.Header
	n := uint32(fs.recordCount())
	ok := h.NextUID != 0 && h.MessagesCount == n &&
		h.SeenMessagesCount <= h.MessagesCount && h.DeletedMessagesCount <= h.MessagesCount
	// The last uid only from a chunk in memory: a base marked as in order
	// is written with its next uid above its last record.
	if ok && n > 0 && (fs.recs == nil || fs.recs.Loaded(fs.recs.Chunks()-1)) {
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
	if fs.recs == nil || fs.loading {
		return nil
	}
	if fs.recs.AllLoaded() {
		return fs.applyPending()
	}
	fs.loading = true
	defer func() { fs.loading = false }()
	if err := fs.loadRecords(); err != nil {
		return err
	}
	if len(fs.pending) > 0 || fs.countersUnknown != 0 {
		return fs.applyPending() // recounts the header too
	}
	fs.reconcileHeaderLocked()
	return nil
}
