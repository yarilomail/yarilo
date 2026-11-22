package file

import (
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"
)

// A fold keeps the log it folded as .log.2, as the reference does, so the
// expunges in it still answer "what vanished since" for a client's state
// issued before the fold (#2664). Its age is capped by mail_index_log2_max_age.
const log2Suffix = ".log.2"

// log2Opens counts the opens of a kept log, so "a read or a delivery never
// opens it" is a number a test reads.
var log2Opens atomic.Int64

// Log2Opens returns the count, and ResetLog2Opens zeroes it.
func Log2Opens() int64         { return log2Opens.Load() }
func ResetLog2Opens()          { log2Opens.Store(0) }
func log2Path(p string) string { return p + log2Suffix }

// renameLog moves the folded log aside. Test seam: a fold that stops between
// the base write and this rename is a state a reader must survive.
var renameLog = os.Rename

// rotateLog starts a new log for lineage after a fold, keeping the folded one
// as .log.2 when keep is set and dropping any kept log otherwise. The base is
// already written: a crash before the rename leaves the folded log in place,
// which the base names as folded.
func rotateLog(indexPath string, indexID, lineage uint32, initial uint64, keep bool) error {
	if keep {
		if err := renameLog(indexPath+".log", log2Path(indexPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("fileindex/log rotate: keep: %w", err)
		}
	} else if err := dropKeptLog(indexPath); err != nil {
		return err
	}
	return truncateLogLineage(indexPath, indexID, lineage, initial)
}

// dropKeptLog removes .log.2: a history that is no longer this folder's.
func dropKeptLog(indexPath string) error {
	if err := os.Remove(log2Path(indexPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("fileindex/log rotate: drop kept log: %w", err)
	}
	return nil
}

// keptLog opens .log.2 when it is the log this base folded and younger than
// maxAge: another lineage is a leftover from an earlier fold, and its expunges
// are not the ones the base's floor counts from. The caller closes it.
func (fs *folderState) keptLog(maxAge time.Duration) (*logReader, bool) {
	if maxAge <= 0 || fs.lineage.FoldedLineage == 0 {
		return nil, false
	}
	log2Opens.Add(1)
	lg, err := openLogFile(log2Path(fs.indexPath))
	if err != nil || lg.f == nil {
		return nil, false
	}
	if !lg.ok || lg.hdr.IndexID != fs.file.Header.IndexID || lg.hdr.FileSeq != fs.lineage.FoldedLineage ||
		time.Since(lg.stat.ModTime()) > maxAge {
		lg.close()
		return nil, false
	}
	return lg, true
}

// keptFloorLocked is the modseq the kept log's history starts at, or zero when
// there is no kept log that says: an older binary's log carries no start.
func (fs *folderState) keptFloorLocked(maxAge time.Duration) uint64 {
	lg, ok := fs.keptLog(maxAge)
	if !ok {
		return 0
	}
	defer lg.close()
	return lg.hdr.InitialModSeq
}

// pruneKeptLogLocked removes a kept log past maxAge, under the folder lock.
func (fs *folderState) pruneKeptLogLocked(maxAge time.Duration) {
	st, err := os.Stat(log2Path(fs.indexPath))
	if err != nil {
		return
	}
	if maxAge > 0 && time.Since(st.ModTime()) <= maxAge {
		return
	}
	_ = os.Remove(log2Path(fs.indexPath))
}

// log2CheckEvery spaces the age checks a writer makes: a stat a write is not
// worth, and the age is days, not seconds.
const log2CheckEvery = 10 * time.Minute

// pruneKeptLogSometimesLocked is pruneKeptLogLocked at most once per
// log2CheckEvery per folder state, under the folder lock.
func (fs *folderState) pruneKeptLogSometimesLocked() {
	if now := time.Now(); now.Sub(fs.log2Checked) >= log2CheckEvery {
		fs.log2Checked = now
		fs.pruneKeptLogLocked(fs.log2MaxAge)
	}
}

// logStartModSeq is the modseq a new log's history starts at: the highest the
// folder has seen, at least 1 from its creation; zero says "not recorded".
func (fs *folderState) logStartModSeq() uint64 {
	m, err := fs.highestModSeq()
	if err != nil {
		return 0
	}
	return m
}
