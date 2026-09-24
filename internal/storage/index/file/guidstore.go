package file

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The per-user GUID store: one record per copy, opened by its own name so no
// folder walk can reach it (#1711, INTERNALS.md §35).
const (
	GUIDIndexFileName = "yarilo.guid.index"
	extNameGUIDMap    = "guidmap"
	guidMapRecSize    = 56
)

// guidMapRec is the record extension, laid out from the Cyrus key and value
// (conversations.c:2306, 2318-2326).
type guidMapRec struct {
	GUID         [16]byte
	FolderGUID   [16]byte
	UID          uint32
	Flags        uint32
	InternalDate int64
	CID          uint64
}

func encodeGUIDMapRec(r guidMapRec) []byte {
	b := make([]byte, guidMapRecSize)
	copy(b[0:16], r.GUID[:])
	copy(b[16:32], r.FolderGUID[:])
	binary.LittleEndian.PutUint32(b[32:36], r.UID)
	binary.LittleEndian.PutUint32(b[36:40], r.Flags)
	binary.LittleEndian.PutUint64(b[40:48], uint64(r.InternalDate))
	binary.LittleEndian.PutUint64(b[48:56], r.CID)
	return b
}

func decodeGUIDMapRec(b []byte) (guidMapRec, bool) {
	if len(b) < guidMapRecSize {
		return guidMapRec{}, false
	}
	var r guidMapRec
	copy(r.GUID[:], b[0:16])
	copy(r.FolderGUID[:], b[16:32])
	r.UID = binary.LittleEndian.Uint32(b[32:36])
	r.Flags = binary.LittleEndian.Uint32(b[36:40])
	r.InternalDate = int64(binary.LittleEndian.Uint64(b[40:48]))
	r.CID = binary.LittleEndian.Uint64(b[48:56])
	return r, true
}

// guidStore is the open handle, one per user index.
type guidStore struct {
	mu      sync.Mutex
	id      uint64
	image   map[[16]byte][]mailbox.GUIDRecord
	version guidImageVersion
}

// guidImageVersion says which image a map was built from: the file's identity,
// how far its uids have gone, and how many records it holds.
type guidImageVersion struct {
	indexID uint32
	nextUID uint32
	records int
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

	// A store that exists is opened, not created: a reader must not take the
	// lock a creation needs.
	intent := intentCreate
	if _, err := os.Stat(path); err == nil {
		intent = intentOpen
	}
	fs := &folderState{
		user:       u.username,
		folder:     "",
		indexDir:   dir,
		indexPath:  path,
		intent:     intent,
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

// errGUIDStoreStale says the store on disk carries an older record shape. It
// is derived, so it is refused rather than reinterpreted, and rebuilt (#1711).
var errGUIDStoreStale = errors.New("fileindex/guid: store written in an older record shape")

func (u *userIndex) declareGUIDExt(fs *folderState) error {
	if ext := findExt(fs.file.Extensions, extNameGUIDMap); ext != nil {
		if ext.RecordSize != guidMapRecSize {
			metricGUIDStoreStale.Inc()
			return fmt.Errorf("%w: records are %d bytes, this build writes %d",
				errGUIDStoreStale, ext.RecordSize, guidMapRecSize)
		}
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
			GUID: r.GUID, FolderGUID: r.FolderGUID, UID: r.UID,
			Flags: r.Flags, InternalDate: r.InternalDate, CID: r.CID,
		})
		return fs.flush()
	})
}

// RemoveGUIDRecords drops every copy the store holds for one uid of one
// folder: an expunge removes the copy, not the message.
func (u *userIndex) RemoveGUIDRecords(folderGUID [16]byte, uid uint32) error {
	id, err := u.openGUIDStore()
	if err != nil {
		return err
	}
	return u.withFolderSite(id, lockSiteGUIDAppend, func(fs *folderState) error {
		var gone []uint32
		for i := range fs.file.Records {
			r, ok := decodeGUIDMapRec(fs.file.Records[i].Ext[extNameGUIDMap])
			if ok && r.FolderGUID == folderGUID && r.UID == uid {
				gone = append(gone, fs.file.Records[i].UID)
			}
		}
		if len(gone) == 0 {
			return nil
		}
		for _, sequence := range gone {
			if _, eerr := fs.expungeLocked(sequence, 0); eerr != nil {
				return eerr
			}
		}
		return fs.flush()
	})
}

// GUIDRecords reads every copy the store holds. A store in an older shape
// reads as empty: it is derived, and its folder field is a number.
func (u *userIndex) GUIDRecords() ([]mailbox.GUIDRecord, error) {
	id, err := u.openGUIDStore()
	if errors.Is(err, errGUIDStoreStale) {
		return nil, nil
	}
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
				GUID: r.GUID, FolderGUID: r.FolderGUID, UID: r.UID,
				Flags: r.Flags, InternalDate: r.InternalDate, CID: r.CID,
			})
		}
		return nil
	})
	return out, err
}

// trackAppendedGUID records one copy written outside a transaction: delivery
// and APPEND settle the uid in folder.go, not in indexTx (#1986).
func (u *userIndex) trackAppendedGUID(fs *folderState, m *mailbox.MessageMeta) {
	if m == nil || m.GUID == ([16]byte{}) {
		return
	}
	u.applyGUIDTracking(fs, guidBatch{add: []mailbox.GUIDRecord{{
		GUID:         m.GUID,
		FolderGUID:   fs.hdr.MailboxGUID,
		UID:          m.UID,
		InternalDate: m.InternalDate.Unix(),
	}}})
}

// trackStampedGUIDs records copies whose identity was settled after the record
// was written; until the store knows one, no search can name it (#1986).
func (u *userIndex) trackStampedGUIDs(fs *folderState, recs []mailbox.GUIDRecord) {
	if len(recs) == 0 {
		return
	}
	u.applyGUIDTracking(fs, guidBatch{add: recs})
}

// trackReplacedFolder makes the store hold exactly what the folder now holds:
// a rebuild replaces records and their identities in one command.
func (u *userIndex) trackReplacedFolder(fs *folderState, recs []mailbox.GUIDRecord) {
	folder := fs.hdr.MailboxGUID
	u.applyGUIDTracking(fs, guidBatch{goneFolder: &folder, add: recs})
}

// trackDeletedFolder retracts every copy of a folder that is gone, or an
// account-wide search answers with a copy in a mailbox that does not exist.
func (u *userIndex) trackDeletedFolder(folder string, folderGUID [16]byte) {
	if folderGUID == ([16]byte{}) {
		return
	}
	if err := u.applyGUIDBatch(guidBatch{goneFolder: &folderGUID}); err != nil {
		metricGUIDTrackFailed.Inc()
		slog.Warn("fileindex: the guid store did not take this command; it is derived and rebuildable",
			"user", u.username, "folder", folder, "err", err)
	}
}

// trackExpungedGUID retracts one copy written outside a transaction.
func (u *userIndex) trackExpungedGUID(fs *folderState, uid uint32) {
	u.applyGUIDTracking(fs, guidBatch{gone: []guidCopy{{folderGUID: fs.hdr.MailboxGUID, uid: uid}}})
}

// applyGUIDTracking is the shared tail: the store is derived, so a failure is
// counted and logged rather than failing mail that is already written.
func (u *userIndex) applyGUIDTracking(fs *folderState, b guidBatch) {
	if err := u.applyGUIDBatch(b); err != nil {
		metricGUIDTrackFailed.Inc()
		slog.Warn("fileindex: the guid store did not take this command; it is derived and rebuildable",
			"user", u.username, "folder", fs.folder, "err", err)
	}
}

// guidCopy names one copy: the folder and the uid inside it.
type guidCopy struct {
	folderGUID [16]byte
	uid        uint32
}

// guidBatch is one command's worth of changes to the store.
type guidBatch struct {
	add  []mailbox.GUIDRecord
	gone []guidCopy
	// goneFolder retracts every copy a folder holds, for the commands that
	// take the folder away rather than one message (#1986).
	goneFolder *[16]byte
}

// applyGUIDBatch takes the store once and writes the command's copies: per-op
// holds cost a transaction one acquisition per record (#1827).
func (u *userIndex) applyGUIDBatch(b guidBatch) error {
	if len(b.add) == 0 && len(b.gone) == 0 && b.goneFolder == nil {
		return nil
	}
	// Nothing to remove from a store that was never written: an expunge must
	// not be what creates it.
	if len(b.add) == 0 {
		if _, err := os.Stat(u.GUIDStorePath()); err != nil {
			return nil
		}
	}
	id, err := u.openGUIDStore()
	if err != nil {
		return err
	}
	return u.withFolderSite(id, lockSiteGUIDAppend, func(fs *folderState) error {
		if b.goneFolder != nil {
			for i := len(fs.file.Records) - 1; i >= 0; i-- {
				r, ok := decodeGUIDMapRec(fs.file.Records[i].Ext[extNameGUIDMap])
				if !ok || r.FolderGUID != *b.goneFolder {
					continue
				}
				if _, eerr := fs.expungeLocked(fs.file.Records[i].UID, 0); eerr != nil {
					return eerr
				}
			}
		}
		for _, c := range b.gone {
			for i := range fs.file.Records {
				r, ok := decodeGUIDMapRec(fs.file.Records[i].Ext[extNameGUIDMap])
				if !ok || r.FolderGUID != c.folderGUID || r.UID != c.uid {
					continue
				}
				if _, eerr := fs.expungeLocked(fs.file.Records[i].UID, 0); eerr != nil {
					return eerr
				}
				break
			}
		}
		for _, r := range b.add {
			if err := appendGUIDLocked(fs, r); err != nil {
				return err
			}
		}
		return fs.flush()
	})
}

func appendGUIDLocked(fs *folderState, r mailbox.GUIDRecord) error {
	uid := fs.file.Header.NextUID
	if uid == 0 {
		uid = 1
	}
	fs.file.Header.NextUID = uid + 1
	if err := fs.appendLocked(&mailbox.MessageMeta{UID: uid}); err != nil {
		return err
	}
	rec := fs.file.Records[len(fs.file.Records)-1]
	if rec.Ext == nil {
		rec.Ext = map[string][]byte{}
	}
	rec.Ext[extNameGUIDMap] = encodeGUIDMapRec(guidMapRec{
		GUID: r.GUID, FolderGUID: r.FolderGUID, UID: r.UID,
		Flags: r.Flags, InternalDate: r.InternalDate, CID: r.CID,
	})
	return nil
}

// GUIDCopies answers from the image the store last read: the map is built once
// per version of the file, so a second lookup in a session costs no I/O.
func (u *userIndex) GUIDCopies(guids [][16]byte) ([]mailbox.GUIDRecord, error) {
	if len(guids) == 0 {
		return nil, nil
	}
	byGUID, err := u.guidImage()
	if err != nil || byGUID == nil {
		return nil, err
	}
	var out []mailbox.GUIDRecord
	for _, g := range guids {
		out = append(out, byGUID[g]...)
	}
	return out, nil
}

// guidImage returns the lookup map for the store as it stands, rebuilding it
// only when the file has moved on.
func (u *userIndex) guidImage() (map[[16]byte][]mailbox.GUIDRecord, error) {
	// A lookup never creates the store: reading is not what brings a derived
	// file into being, and an empty answer sends the caller to the walk.
	if _, err := os.Stat(u.GUIDStorePath()); err != nil {
		return nil, nil
	}
	id, err := u.openGUIDStore()
	if errors.Is(err, errGUIDStoreStale) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var version guidImageVersion
	if err := u.withFolderROUnlocked(id, func(fs *folderState) error {
		version = guidImageVersion{
			indexID: fs.file.Header.IndexID,
			nextUID: fs.file.Header.NextUID,
			records: len(fs.file.Records),
		}
		return nil
	}); err != nil {
		return nil, err
	}

	u.guid.mu.Lock()
	if u.guid.image != nil && u.guid.version == version {
		m := u.guid.image
		u.guid.mu.Unlock()
		metricGUIDImageHit.Inc()
		return m, nil
	}
	u.guid.mu.Unlock()

	recs, err := u.GUIDRecords()
	if err != nil {
		return nil, err
	}
	m := make(map[[16]byte][]mailbox.GUIDRecord, len(recs))
	for _, r := range recs {
		m[r.GUID] = append(m[r.GUID], r)
	}
	u.guid.mu.Lock()
	u.guid.image, u.guid.version = m, version
	u.guid.mu.Unlock()
	metricGUIDImageBuilt.Inc()
	return m, nil
}

// ReplaceGUIDStore writes the store from scratch: the file is derived, so this
// is how one that is behind, refused or missing comes back (#1711).
func (u *userIndex) ReplaceGUIDStore(copies []mailbox.GUIDRecord) error {
	dir := u.indexRootDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("fileindex/guid-rebuild: mkdir: %w", err)
	}
	// The set a fresh store is created with, plus ours: a file carrying only
	// the guidmap extension refuses the next append, which loses the copy.
	exts := append(defaultExtensions(1, [16]byte{}), mailindex.Extension{
		Name: extNameGUIDMap, RecordSize: guidMapRecSize, RecordAlign: 8, ResetID: 1,
	})
	layout, err := mailindex.ComputeRecordLayout(exts)
	if err != nil {
		return fmt.Errorf("fileindex/guid-rebuild: layout: %w", err)
	}
	extBytes, err := mailindex.EncodeExtHeaders(layout.Extensions)
	if err != nil {
		return fmt.Errorf("fileindex/guid-rebuild: extension headers: %w", err)
	}
	hdr := mailindex.NewHeader(uint32(time.Now().Unix()))
	hdr.UIDValidity = 1
	hdr.RecordSize = layout.RecordSize
	hdr.HeaderSize = uint32(mailindex.HeaderMinSize) + uint32(len(extBytes))
	hdr.NextUID = uint32(len(copies)) + 1

	records := make([]*mailindex.Record, 0, len(copies))
	for i, c := range copies {
		records = append(records, &mailindex.Record{
			UID: uint32(i + 1),
			Ext: map[string][]byte{extNameGUIDMap: encodeGUIDMapRec(guidMapRec{
				GUID: c.GUID, FolderGUID: c.FolderGUID, UID: c.UID,
				Flags: c.Flags, InternalDate: c.InternalDate, CID: c.CID,
			})},
		})
	}

	u.guid.mu.Lock()
	defer u.guid.mu.Unlock()
	if _, err := mailindex.Recreate(mailindex.RecreateInput{
		Path: u.GUIDStorePath(), Header: hdr, Extensions: layout.Extensions,
		Records: records,
	}); err != nil {
		return fmt.Errorf("fileindex/guid-rebuild: write: %w", err)
	}
	// Hygiene, not correctness: the new file has its own index id and the old
	// log is not folded into it, but a log naming a file nobody has is litter.
	if err := os.Remove(u.GUIDStorePath() + ".log"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("fileindex/guid-rebuild: drop log: %w", err)
	}
	// The handle and the map built from the file that is gone: a reader that
	// keeps them answers from a store nobody has any more (#1986).
	id := u.guid.id
	u.guid.id = 0
	u.guid.image, u.guid.version = nil, guidImageVersion{}
	if id != 0 {
		u.mu.Lock()
		delete(u.open, id)
		u.mu.Unlock()
	}
	metricGUIDRebuilt.Inc()
	return nil
}
