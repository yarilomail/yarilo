package file

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The per-user GUID store: one record per copy, opened by its own name so no
// folder walk can reach it (#1711, INTERNALS.md §35).
const (
	GUIDIndexFileName = "yarilo.guid.index"
	extNameGUIDMap    = "guidmap"
	guidMapRecSize    = 48
)

// guidMapRec is the record extension, laid out from the Cyrus key and value
// (conversations.c:2306, 2318-2326).
type guidMapRec struct {
	GUID         [16]byte
	FolderID     uint64
	UID          uint32
	Flags        uint32
	InternalDate int64
	CID          uint64
}

func encodeGUIDMapRec(r guidMapRec) []byte {
	b := make([]byte, guidMapRecSize)
	copy(b[0:16], r.GUID[:])
	binary.LittleEndian.PutUint64(b[16:24], r.FolderID)
	binary.LittleEndian.PutUint32(b[24:28], r.UID)
	binary.LittleEndian.PutUint32(b[28:32], r.Flags)
	binary.LittleEndian.PutUint64(b[32:40], uint64(r.InternalDate))
	binary.LittleEndian.PutUint64(b[40:48], r.CID)
	return b
}

func decodeGUIDMapRec(b []byte) (guidMapRec, bool) {
	if len(b) < guidMapRecSize {
		return guidMapRec{}, false
	}
	var r guidMapRec
	copy(r.GUID[:], b[0:16])
	r.FolderID = binary.LittleEndian.Uint64(b[16:24])
	r.UID = binary.LittleEndian.Uint32(b[24:28])
	r.Flags = binary.LittleEndian.Uint32(b[28:32])
	r.InternalDate = int64(binary.LittleEndian.Uint64(b[32:40]))
	r.CID = binary.LittleEndian.Uint64(b[40:48])
	return r, true
}

// guidStore is the open handle, one per user index.
type guidStore struct {
	mu sync.Mutex
	id uint64
}

// GUIDStorePath is where the store lives: the index root the folders use, so
// INDEX= moves it with them.
func (u *userIndex) GUIDStorePath() string {
	return filepath.Join(u.indexRootDir(), GUIDIndexFileName)
}

// openGUIDStore opens or creates it, by file name rather than by folder.
func (u *userIndex) openGUIDStore() (uint64, error) {
	u.guid.mu.Lock()
	defer u.guid.mu.Unlock()
	if u.guid.id != 0 {
		return u.guid.id, nil
	}
	dir := u.indexRootDir()
	path := u.GUIDStorePath()
	if u.b.noCreate {
		if _, err := os.Stat(path); err != nil {
			return 0, fmt.Errorf("fileindex/guid: no store at %s: %w", path, err)
		}
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, fmt.Errorf("fileindex/guid: mkdir: %w", err)
	}

	u.mu.Lock()
	u.next++
	id := u.next
	u.mu.Unlock()

	fs := &folderState{
		user:       u.username,
		folder:     "",
		indexDir:   dir,
		indexPath:  path,
		intent:     intentCreate,
		lockMethod: u.b.lockMethod,
		fsync:      u.b.fsync,
	}
	// UIDVALIDITY 1: the record uid here is a sequence number of copies, not a
	// message uid, and nothing outside this file reads it.
	if err := u.loadOrInit(fs, 1); err != nil {
		return 0, err
	}
	if err := u.declareGUIDExt(fs); err != nil {
		return 0, err
	}
	u.mu.Lock()
	u.open[id] = fs
	u.mu.Unlock()
	u.guid.id = id
	return id, nil
}

func (u *userIndex) declareGUIDExt(fs *folderState) error {
	if findExt(fs.file.Extensions, extNameGUIDMap) != nil {
		return nil
	}
	if err := fs.file.AddRecordExtension(extNameGUIDMap, nil, guidMapRecSize, 8, fs.file.Header.UIDValidity); err != nil {
		return fmt.Errorf("fileindex/guid: declare extension: %w", err)
	}
	return fs.flush()
}

// AppendGUIDRecord records one copy of one message.
func (u *userIndex) AppendGUIDRecord(r mailbox.GUIDRecord) error {
	id, err := u.openGUIDStore()
	if err != nil {
		return err
	}
	return u.withFolderSite(id, lockSiteGUIDAppend, func(fs *folderState) error {
		uid := fs.file.Header.NextUID
		if uid == 0 {
			uid = 1
		}
		fs.file.Header.NextUID = uid + 1
		m := &mailbox.MessageMeta{UID: uid}
		if err := fs.appendLocked(m); err != nil {
			return err
		}
		rec := fs.file.Records[len(fs.file.Records)-1]
		if rec.Ext == nil {
			rec.Ext = map[string][]byte{}
		}
		rec.Ext[extNameGUIDMap] = encodeGUIDMapRec(guidMapRec{
			GUID: r.GUID, FolderID: r.FolderID, UID: r.UID,
			Flags: r.Flags, InternalDate: r.InternalDate, CID: r.CID,
		})
		return fs.flush()
	})
}

// GUIDRecords reads every copy the store holds, in the order it recorded them.
func (u *userIndex) GUIDRecords() ([]mailbox.GUIDRecord, error) {
	id, err := u.openGUIDStore()
	if err != nil {
		return nil, err
	}
	var out []mailbox.GUIDRecord
	err = u.withFolderROUnlocked(id, func(fs *folderState) error {
		out = out[:0]
		for i := range fs.file.Records {
			r, ok := decodeGUIDMapRec(fs.file.Records[i].Ext[extNameGUIDMap])
			if !ok {
				continue
			}
			out = append(out, mailbox.GUIDRecord{
				GUID: r.GUID, FolderID: r.FolderID, UID: r.UID,
				Flags: r.Flags, InternalDate: r.InternalDate, CID: r.CID,
			})
		}
		return nil
	})
	return out, err
}
