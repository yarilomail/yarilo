package file

import (
	"log/slog"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// metricStaleBase counts a writer that found, under the journal hold and
// after its reload, a state that does not match the files: a base on disk
// other than the one it holds, or a header its own records disagree with. It
// was read again before it wrote. The labels carry the diagnosis (#2591).
var metricStaleBase = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "fileindex_stale_base_total",
	Help: "Writes that found a state not matching the files and read it again first, by site, by how the state took its base, by check (base: another base on disk; counters: a header its own records disagree with) and by whether the base file's inode, size and mtime still matched the state's.",
}, []string{"site", "from", "check", "file_same"})

// verifyBaseLocked holds a writer to the base on disk. Under the journal hold
// no other writer replaces it, so after the reload the base this state took
// must be the one the path names: lineage and records digest from its
// header, read without parsing the file. Another base means the reload missed
// a change; the state is read again in full before anything is written.
// Caller holds fs.mu and the journal.
func (fs *folderState) verifyBaseLocked(site string) error {
	if fs.file == nil || fs.lineage.Lineage == lineageUnknown {
		return nil
	}
	disk, err := peekLineage(fs.indexPath)
	if err != nil || disk.Lineage == lineageUnknown {
		return nil
	}
	if disk.Lineage == fs.lineage.Lineage && disk.RecordsDigest == fs.lineage.RecordsDigest {
		return nil
	}
	same := fs.baseFileSame()
	metricStaleBase.WithLabelValues(site, fs.baseFrom, "base", same).Inc()
	slog.Debug("fileindex: a writer held another base than the one on disk; read again",
		"user", fs.user, "folder", fs.folder, "site", site, "from", fs.baseFrom, "file_same", same,
		"held_lineage", fs.lineage.Lineage, "held_digest", fs.lineage.RecordsDigest,
		"disk_lineage", disk.Lineage, "disk_digest", disk.RecordsDigest, "log_size", fs.logSize)
	fs.baseMod, fs.baseIdent = time.Time{}, nil
	fs.recordsLeft = true
	return fs.reload()
}

// baseFileSame says whether the base file still shows the inode, size and
// mtime this state recorded: "yes" with another base inside is a reused
// inode, the case only an open descriptor rules out.
func (fs *folderState) baseFileSame() string {
	st, err := os.Stat(fs.indexPath)
	if err != nil || fs.baseIdent == nil {
		return "unknown"
	}
	if os.SameFile(fs.baseIdent, st) && st.Size() == fs.baseIdent.Size() && st.ModTime().Equal(fs.baseMod) {
		return "yes"
	}
	return "no"
}

// verifyCountersLocked holds a writer's header to its own records once all
// are loaded: a header they disagree with would be journaled as the next
// absolute counters. Taken from the records, counted, Debug. Caller holds
// fs.mu.
func (fs *folderState) verifyCountersLocked(site string) {
	if fs.recs == nil || !fs.recs.AllLoaded() || len(fs.pending) > 0 || fs.countersUnknown != 0 {
		return
	}
	_, seen, deleted := fs.countRecordsLocked()
	h := &fs.file.Header
	n := uint32(fs.recordCount())
	if h.MessagesCount == n && h.SeenMessagesCount == seen && h.DeletedMessagesCount == deleted {
		return
	}
	metricStaleBase.WithLabelValues(site, fs.baseFrom, "counters", fs.baseFileSame()).Inc()
	slog.Debug("fileindex: a writer's header disagreed with its records; taken from the records",
		"user", fs.user, "folder", fs.folder, "site", site, "from", fs.baseFrom,
		"messages", h.MessagesCount, "seen", h.SeenMessagesCount, "deleted", h.DeletedMessagesCount,
		"records", n, "records_seen", seen, "records_deleted", deleted, "log_size", fs.logSize)
	h.MessagesCount, h.SeenMessagesCount, h.DeletedMessagesCount = n, seen, deleted
}
