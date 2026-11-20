package file

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/yarilomail/yarilo/v2/internal/sortedfile"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// The GUID lookup beside the store: its base's copies by GUID, searched on
// disk (INDEX-MEMORY.md §5a). The journal past that base is read from the
// store's pending records, so neither a lookup nor an append holds or
// rebuilds the store in memory; a new base, written when its journal folds,
// gets a new lookup at the next read.
const (
	guidLookupSuffix  = ".lookup"
	guidLookupSection = 1
	// guidLookupEntry is one copy in a value: the store uid, then its record.
	guidLookupEntry = 4 + guidMapRecSize
)

// guidLookupBuildSeam is a test seam: runs as a lookup is built.
var guidLookupBuildSeam func()

// guidBaseTag names one base of the store: its index id and lineage.
func guidBaseTag(indexID, lineage uint32) uint64 { return uint64(indexID)<<32 | uint64(lineage) }

// guidLookup opens the lookup for the store's base tagged tag, building it
// when the one on disk describes another base.
func (u *userIndex) guidLookup(tag uint64) (*sortedfile.Reader, error) {
	u.guid.mu.Lock()
	if r := u.guid.lookup; r != nil && r.Generation == tag {
		u.guid.mu.Unlock()
		metricGUIDImageHit.Inc()
		return r, nil
	}
	u.guid.mu.Unlock()
	path := u.GUIDStorePath() + guidLookupSuffix
	r, err := sortedfile.Open(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, sortedfile.ErrMoved) {
		return nil, fmt.Errorf("fileindex/guid-lookup: %w", err)
	}
	if r == nil || r.Generation != tag {
		if r != nil {
			r.Close() //nolint:errcheck
		}
		if err := u.buildGUIDLookup(path); err != nil {
			return nil, err
		}
		if r, err = sortedfile.Open(path); err != nil {
			return nil, fmt.Errorf("fileindex/guid-lookup: %w", err)
		}
		if r.Generation != tag {
			// The base moved while it was read: the next read builds again.
			r.Close() //nolint:errcheck
			return nil, errGUIDBaseMoved
		}
	}
	u.guid.mu.Lock()
	defer u.guid.mu.Unlock()
	if old := u.guid.lookup; old != nil {
		old.Close() //nolint:errcheck
	}
	u.guid.lookup = r
	return r, nil
}

// errGUIDNoChunks is a view holding decoded records: answered by a read of all.
var errGUIDNoChunks = errors.New("fileindex/guid-lookup: the store's view has no chunks")

// errGUIDBaseMoved is a base replaced under a lookup's build or read.
var errGUIDBaseMoved = errors.New("fileindex/guid-lookup: the store's base moved")

// buildGUIDLookup writes the lookup from the store's base as it is on disk,
// read through records of its own: the shared image's chunks stay unloaded.
func (u *userIndex) buildGUIDLookup(path string) error {
	if guidLookupBuildSeam != nil {
		guidLookupBuildSeam()
	}
	parsed, recs, err := mailindex.OpenLazy(u.GUIDStorePath())
	if err != nil {
		return fmt.Errorf("fileindex/guid-lookup: base: %w", err)
	}
	if err := recs.LoadAll(); err != nil {
		return fmt.Errorf("fileindex/guid-lookup: base: %w", err)
	}
	id, ok := recs.Layout().ExtID(extNameGUIDMap)
	type entry struct {
		guid [16]byte
		raw  [guidLookupEntry]byte
	}
	entries := make([]entry, 0, recs.Len())
	if ok {
		for i := range recs.Len() {
			data := recs.Ext(i, id)
			var e entry
			copy(e.guid[:], data[:16])
			binary.LittleEndian.PutUint32(e.raw[:4], recs.UID(i))
			copy(e.raw[4:], data)
			entries = append(entries, e)
		}
	}
	slices.SortFunc(entries, func(a, b entry) int {
		if c := bytes.Compare(a.guid[:], b.guid[:]); c != 0 {
			return c
		}
		return bytes.Compare(a.raw[:4], b.raw[:4])
	})
	var kvs []sortedfile.KV
	for i := 0; i < len(entries); {
		j := i
		var val []byte
		for ; j < len(entries) && entries[j].guid == entries[i].guid; j++ {
			val = append(val, entries[j].raw[:]...)
		}
		kvs = append(kvs, sortedfile.KV{Key: append([]byte(nil), entries[i].guid[:]...), Val: val})
		i = j
	}
	tag := guidBaseTag(parsed.Header.IndexID, readLineage(parsed).Lineage)
	w, err := sortedfile.Create(path, tag, uint64(parsed.Header.NextUID), []uint32{guidLookupSection}, false)
	if err != nil {
		return fmt.Errorf("fileindex/guid-lookup: %w", err)
	}
	if err := sortedfile.MergeSection(w, nil, guidLookupSection, sortedfile.SortKVs(kvs)); err != nil {
		w.Abort()
		return fmt.Errorf("fileindex/guid-lookup: %w", err)
	}
	if err := w.Commit(); err != nil {
		return fmt.Errorf("fileindex/guid-lookup: %w", err)
	}
	metricGUIDImageBuilt.Inc()
	return nil
}

// guidCopiesFrom answers guids from the store's view fs: the base's copies
// from the lookup, less what the journal expunged, plus what it appended.
func (u *userIndex) guidCopiesFrom(fs *folderState, guids [][16]byte) ([]mailbox.GUIDRecord, error) {
	if fs.recs == nil {
		return nil, errGUIDNoChunks
	}
	look, err := u.guidLookup(guidBaseTag(fs.file.Header.IndexID, fs.lineage.Lineage))
	if err != nil {
		return nil, err
	}
	want := make(map[[16]byte]bool, len(guids))
	for _, g := range guids {
		want[g] = true
	}
	absorbed := uint32(look.Absorbed)
	gone := map[uint32]bool{}
	var tail []tailCopy
	if fs.recs != nil && fs.recs.AllLoaded() {
		// The journal is applied to records already in memory: what is not
		// there is gone, and the records past the base are the tail.
		present := func(uid uint32) bool { _, ok := fs.recs.SeqOfUID(uid); return ok }
		from, _ := fs.recs.SeqOfUID(absorbed)
		id, ok := fs.recs.Layout().ExtID(extNameGUIDMap)
		for i := from; ok && i < fs.recs.Len(); i++ {
			if r, ok := decodeGUIDMapRec(fs.recs.Ext(i, id)); ok && want[r.GUID] {
				tail = append(tail, tailCopy{uid: fs.recs.UID(i), rec: guidRecordOf(r)})
			}
		}
		return u.guidBaseCopies(look, guids, func(uid uint32) bool { return !present(uid) }, tail)
	}
	stride := int(fs.recs.Layout().RecordSize)
	at := mapOffset(fs)
	for _, rec := range fs.pending {
		hdr, err := mailindex.DecodeTxHeader(rec)
		if err != nil {
			return nil, err
		}
		switch payload := rec[8:hdr.Size]; hdr.Type.Kind() {
		case mailindex.TxTypeAppend:
			for i := 0; stride > 0 && at >= 0 && i+stride <= len(payload); i += stride {
				if r, ok := decodeGUIDMapRec(payload[i+at : i+at+guidMapRecSize]); ok && want[r.GUID] {
					tail = append(tail, tailCopy{uid: binary.LittleEndian.Uint32(payload[i:]), rec: guidRecordOf(r)})
				}
			}
		case mailindex.TxTypeExpungeGUID | mailindex.TxType(mailindex.TxExpungeProt):
			for _, uid := range expungedUIDs(payload) {
				gone[uid] = true
			}
		}
	}
	return u.guidBaseCopies(look, guids, func(uid uint32) bool { return gone[uid] }, tail)
}

// guidBaseCopies is, per guid, the lookup's copies and then the tail's, none
// gone.
// tailCopy is a copy the journal appended, with its uid in the store.
type tailCopy struct {
	uid uint32
	rec mailbox.GUIDRecord
}

func (u *userIndex) guidBaseCopies(look *sortedfile.Reader, guids [][16]byte, gone func(uint32) bool, tail []tailCopy) ([]mailbox.GUIDRecord, error) {
	var out []mailbox.GUIDRecord
	for _, g := range guids {
		// The base's copies, then the journal's, as one fold answered.
		val, ok, err := look.Get(guidLookupSection, g[:])
		if err != nil {
			if errors.Is(err, sortedfile.ErrMoved) {
				return nil, errGUIDBaseMoved
			}
			return nil, fmt.Errorf("fileindex/guid-lookup: %w", err)
		}
		for i := 0; ok && i+guidLookupEntry <= len(val); i += guidLookupEntry {
			if gone(binary.LittleEndian.Uint32(val[i:])) {
				continue
			}
			if r, ok := decodeGUIDMapRec(val[i+4 : i+guidLookupEntry]); ok {
				out = append(out, guidRecordOf(r))
			}
		}
		for _, c := range tail {
			if c.rec.GUID == g && !gone(c.uid) {
				out = append(out, c.rec)
			}
		}
	}
	return out, nil
}

// mapOffset is where the guidmap extension sits in a record, or -1.
func mapOffset(fs *folderState) int {
	if fs.recs == nil {
		return -1
	}
	l := fs.recs.Layout()
	id, ok := l.ExtID(extNameGUIDMap)
	if !ok {
		return -1
	}
	return int(l.Extensions[id].RecordOffset)
}

func guidRecordOf(r guidMapRec) mailbox.GUIDRecord {
	return mailbox.GUIDRecord{GUID: r.GUID, FolderGUID: r.FolderGUID, UID: r.UID,
		Flags: r.Flags, InternalDate: r.InternalDate, CID: r.CID}
}
