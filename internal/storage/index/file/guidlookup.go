package file

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
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

// lookupRef is an open lookup and the reads using it: one replaced closes
// when the last of them is done.
type lookupRef struct {
	r       *sortedfile.Reader
	refs    int
	retired bool
}

// release ends one read of ref, under u.guid.mu.
func (ref *lookupRef) releaseLocked() {
	ref.refs--
	if ref.retired && ref.refs == 0 {
		ref.r.Close() //nolint:errcheck
	}
}

// retireLocked drops the index's lookup, closing it once no read uses it.
func (g *guidStore) retireLookupLocked() {
	if ref := g.lookup; ref != nil {
		g.lookup = nil
		ref.retired = true
		ref.refs++
		ref.releaseLocked()
	}
}

// guidLookup opens the lookup for the store's base tagged tag, building it
// when the file describes another base or does not open as a lookup: it is
// derived, and a torn one is rebuilt. release ends the read.
func (u *userIndex) guidLookup(tag uint64) (r *sortedfile.Reader, release func(), err error) {
	u.guid.mu.Lock()
	if ref := u.guid.lookup; ref != nil && ref.r.Generation == tag {
		ref.refs++
		u.guid.mu.Unlock()
		metricGUIDImageHit.Inc()
		return ref.r, u.releaser(ref), nil
	}
	u.guid.mu.Unlock()
	path := u.GUIDStorePath() + guidLookupSuffix
	r, oerr := sortedfile.Open(path)
	if oerr != nil || r.Generation != tag {
		if r != nil {
			r.Close() //nolint:errcheck
		}
		if err := u.buildGUIDLookup(path); err != nil {
			// The scan answers instead: a GUID with more copies than one
			// value holds, or a base that would not read.
			slog.Warn("fileindex: GUID lookup not built; answered by a scan", "path", path, "err", err)
			return nil, nil, errGUIDNoChunks
		}
		if r, err = sortedfile.Open(path); err != nil {
			return nil, nil, fmt.Errorf("fileindex/guid-lookup: %w", err)
		}
		if r.Generation != tag {
			// The base moved while it was read: the next read builds again.
			r.Close() //nolint:errcheck
			return nil, nil, errGUIDBaseMoved
		}
	}
	u.guid.mu.Lock()
	defer u.guid.mu.Unlock()
	u.guid.retireLookupLocked()
	ref := &lookupRef{r: r, refs: 1}
	u.guid.lookup = ref
	return r, u.releaser(ref), nil
}

func (u *userIndex) releaser(ref *lookupRef) func() {
	return func() {
		u.guid.mu.Lock()
		defer u.guid.mu.Unlock()
		ref.releaseLocked()
	}
}

// dropGUIDLookup removes a lookup that answered an error: the next read
// builds it from the base.
func (u *userIndex) dropGUIDLookup() {
	u.guid.mu.Lock()
	u.guid.retireLookupLocked()
	u.guid.mu.Unlock()
	if err := os.Remove(u.GUIDStorePath() + guidLookupSuffix); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("fileindex: GUID lookup not removed", "path", u.GUIDStorePath()+guidLookupSuffix, "err", err)
	}
}

// errGUIDNoChunks is a view holding decoded records: answered by a read of all.
var errGUIDNoChunks = errors.New("fileindex/guid-lookup: the store's view has no chunks")

// errGUIDLookupUnread is a lookup whose read failed: dropped and rebuilt.
var errGUIDLookupUnread = errors.New("fileindex/guid-lookup: the lookup does not read")

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
	look, release, err := u.guidLookup(guidBaseTag(fs.file.Header.IndexID, fs.lineage.Lineage))
	if err != nil {
		return nil, err
	}
	defer release()
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
			// A lookup that does not read is rebuilt, not an error for good.
			return nil, fmt.Errorf("%w: %w", errGUIDLookupUnread, err)
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

// mergeGUIDLookup brings the lookup of the store's previous base to the one
// just written, by one streaming merge with what the folded journal appended
// and expunged (reviewer, 2026-11-20): a fold reads no record of the store.
// A lookup of another base, or a journal it cannot pair, is removed instead,
// and the next read builds it whole.
func (fs *folderState) mergeGUIDLookup(prev, next lineageHdr, foldedEnd int64) {
	path := fs.indexPath + guidLookupSuffix
	if guidLookupMergeSeam != nil && guidLookupMergeSeam() {
		return
	}
	if err := fs.mergeGUIDLookupErr(path, prev, next, foldedEnd); err != nil {
		if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			slog.Warn("fileindex: GUID lookup not removed", "path", path, "err", rerr)
		}
		if !errors.Is(err, errGUIDLookupNotMerged) {
			slog.Warn("fileindex: GUID lookup not merged; the next read builds it", "path", path, "err", err)
		}
	}
}

// guidLookupMergeSeam is a test seam: runs as a fold merges the lookup, and
// true skips the merge.
var guidLookupMergeSeam func() bool

// errGUIDLookupNotMerged is nothing to merge with: no lookup of that base.
var errGUIDLookupNotMerged = errors.New("fileindex/guid-lookup: nothing to merge with")

func (fs *folderState) mergeGUIDLookupErr(path string, prev, next lineageHdr, foldedEnd int64) error {
	old, err := sortedfile.Open(path)
	if err != nil {
		return errGUIDLookupNotMerged
	}
	defer old.Close() //nolint:errcheck
	indexID := fs.file.Header.IndexID
	if old.Generation != guidBaseTag(indexID, prev.Lineage) {
		return errGUIDLookupNotMerged
	}
	added, gone, err := fs.foldedGUIDChanges(prev, foldedEnd)
	if err != nil {
		return err
	}
	// Updates are the GUIDs the journal appended: what the old lookup had of
	// them and the new copies. An expunge drops its copy wherever it is.
	var updates []sortedfile.KV
	for g, entries := range added {
		val, _, gerr := old.Get(guidLookupSection, g[:])
		if gerr != nil {
			return gerr
		}
		updates = append(updates, sortedfile.KV{Key: append([]byte(nil), g[:]...), Val: append(append([]byte(nil), val...), entries...)})
	}
	var scratch []byte
	filter := func(_, v []byte) ([]byte, bool) {
		if len(gone) == 0 {
			return v, true
		}
		scratch = scratch[:0]
		dropped := false
		for i := 0; i+guidLookupEntry <= len(v); i += guidLookupEntry {
			if gone[binary.LittleEndian.Uint32(v[i:])] {
				dropped = true
				continue
			}
			scratch = append(scratch, v[i:i+guidLookupEntry]...)
		}
		if !dropped {
			return v, true
		}
		return scratch, len(scratch) > 0
	}
	w, err := sortedfile.Create(path, guidBaseTag(indexID, next.Lineage), uint64(fs.file.Header.NextUID), []uint32{guidLookupSection}, false)
	if err != nil {
		return err
	}
	if err := sortedfile.MergeSectionMapped(w, old, guidLookupSection, sortedfile.SortKVs(updates), filter); err != nil {
		w.Abort()
		return err
	}
	if err := w.Commit(); err != nil {
		return err
	}
	metricGUIDLookupMerged.Inc()
	return nil
}

// foldedGUIDChanges reads the journal the new base absorbed, from where it
// met the previous base to foldedEnd: the copies appended, as lookup entries
// by GUID, and the store uids expunged.
func (fs *folderState) foldedGUIDChanges(prev lineageHdr, foldedEnd int64) (map[[16]byte][]byte, map[uint32]bool, error) {
	lg, err := openLogRead(fs.indexPath)
	if err != nil {
		return nil, nil, err
	}
	defer lg.close()
	if !lg.ok {
		return nil, nil, nil // nothing journalled: the bases hold the same
	}
	from, paired := replayStart(prev, lg.lineage())
	if !paired {
		return nil, nil, errGUIDLookupNotMerged
	}
	tail, err := readTail(lg.ra, from, min(foldedEnd, lg.size))
	if err != nil {
		return nil, nil, err
	}
	at := mapOffset(fs)
	stride := 0
	if fs.recs != nil {
		stride = int(fs.recs.Layout().RecordSize)
	}
	if at < 0 || stride == 0 {
		return nil, nil, errGUIDLookupNotMerged
	}
	added := map[[16]byte][]byte{}
	gone := map[uint32]bool{}
	for pos := 0; pos+8 <= len(tail); {
		hdr, herr := mailindex.DecodeTxHeader(tail[pos:])
		if herr != nil || int(hdr.Size) < 8 || pos+int(hdr.Size) > len(tail) {
			break // a torn end: the base absorbed no further
		}
		payload := tail[pos+8 : pos+int(hdr.Size)]
		pos += int(hdr.Size)
		switch hdr.Type.Kind() {
		case mailindex.TxTypeAppend:
			for i := 0; i+stride <= len(payload); i += stride {
				var g [16]byte
				copy(g[:], payload[i+at:])
				e := make([]byte, 0, guidLookupEntry)
				e = append(e, payload[i:i+4]...)
				added[g] = append(added[g], append(e, payload[i+at:i+at+guidMapRecSize]...)...)
			}
		case mailindex.TxTypeExpungeGUID | mailindex.TxType(mailindex.TxExpungeProt):
			for _, uid := range expungedUIDs(payload) {
				gone[uid] = true
			}
		case mailindex.TxTypeExtIntro, mailindex.TxTypeExtReset:
			// Another layout from here on: the lookup is built whole.
			return nil, nil, errGUIDLookupNotMerged
		}
	}
	return added, gone, nil
}
