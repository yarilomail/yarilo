package file

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxconv"
	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// adoptForeignCache takes their cache file with the folder: its records are
// the same bytes ours would be (#1714), so deleting it would buy a cold
// listing per folder for nothing.
//
// The file keeps its records and field table and gets our pair in its header;
// every record that parses is checksummed, so an adopted mailbox is protected
// from the first read rather than from its first write.
func (u *userIndex) adoptForeignCache(fs *folderState, foreignDir string, metas []*mailbox.MessageMeta) {
	src := dboxconv.ForeignCachePath(foreignDir)
	if _, err := os.Stat(src); err != nil {
		u.dropCacheOffsets(metas)
		return
	}
	dst := filepath.Join(fs.indexDir, mailindex.CacheFileName)
	if err := os.Rename(src, dst); err != nil {
		slog.Warn("fileindex: could not adopt the cache beside a converted folder",
			"user", u.username, "folder", fs.folder, "err", err)
		u.dropCacheOffsets(metas)
		return
	}
	fileSeq := fs.file.Header.UIDValidity
	if err := repairCachePair(dst, fs.file.Header.IndexID, fileSeq); err != nil {
		slog.Warn("fileindex: adopted cache could not be paired; discarding it",
			"user", u.username, "folder", fs.folder, "err", err)
		_ = os.Remove(dst)
		u.dropCacheOffsets(metas)
		return
	}
	cf, err := mailindex.OpenCache(dst, fs.file.Header.IndexID, fileSeq)
	if err != nil {
		slog.Warn("fileindex: adopted cache did not open; discarding it",
			"user", u.username, "folder", fs.folder, "err", err)
		_ = os.Remove(dst)
		u.dropCacheOffsets(metas)
		return
	}
	defer cf.Close() //nolint:errcheck

	// One pass, one crc32 per record: megabytes in milliseconds, once per
	// mailbox, and without it every adopted record is read unchecked forever.
	var kept int
	for _, m := range metas {
		if m == nil || m.CacheOffset == 0 {
			continue
		}
		vals, rerr := cf.ReadRecord(m.CacheOffset)
		if rerr != nil {
			m.CacheOffset, m.CacheCRC = 0, 0
			continue
		}
		m.CacheCRC = mailindex.RecordCRC(vals)
		kept++
	}
	slog.Info("fileindex: adopted the cache of a converted folder",
		"user", u.username, "folder", fs.folder, "records", kept)
}

func (u *userIndex) dropCacheOffsets(metas []*mailbox.MessageMeta) {
	for _, m := range metas {
		if m != nil {
			m.CacheOffset, m.CacheCRC = 0, 0
		}
	}
}

// repairCachePair writes our index identity into the header of a cache file
// written against theirs. Nothing else in the file is touched: the records and
// the field table are what make it worth keeping.
func repairCachePair(path string, indexID, fileSeq uint32) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck
	var b [8]byte
	binary.LittleEndian.PutUint32(b[0:], indexID)
	binary.LittleEndian.PutUint32(b[4:], fileSeq)
	if _, err := f.WriteAt(b[:], 4); err != nil {
		return fmt.Errorf("fileindex/adopt: write cache pair: %w", err)
	}
	if err := f.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return fmt.Errorf("fileindex/adopt: sync cache: %w", err)
	}
	return nil
}

// stampAdoptedCache puts the offsets and checksums the adoption computed into
// the records, in the same critical section that wrote them.
func (fs *folderState) stampAdoptedCache(metas []*mailbox.MessageMeta) error {
	stamps := make(map[uint32]mailbox.CacheStamp, len(metas))
	for _, m := range metas {
		if m != nil && m.CacheOffset != 0 {
			stamps[m.UID] = mailbox.CacheStamp{Offset: m.CacheOffset, CRC: m.CacheCRC}
		}
	}
	if len(stamps) == 0 {
		return nil
	}
	if err := fs.declareRecordExtLocked(extNameCache, nil,
		cacheRecSize, 4, fs.file.Header.UIDValidity); err != nil {
		return err
	}
	if err := fs.declareRecordExtLocked(extNameCacheCRC, nil,
		cacheCRCRecSize, 4, fs.file.Header.UIDValidity); err != nil {
		return err
	}
	for _, rec := range fs.file.Records {
		stamp, ok := stamps[rec.UID]
		if !ok {
			continue
		}
		if rec.Ext == nil {
			rec.Ext = make(map[string][]byte, 2)
		}
		rec.Ext[extNameCache] = encodeCacheRec(stamp.Offset)
		rec.Ext[extNameCacheCRC] = encodeCacheRec(stamp.CRC)
	}
	return nil
}
