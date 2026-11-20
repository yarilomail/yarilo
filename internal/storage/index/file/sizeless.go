package file

import (
	"encoding/binary"
	"sort"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// SizelessUIDs are the records carrying no virtual size, above the highest uid
// the folder's vsize aggregate counted: only those can be missing from it, so
// a delivery's quota check reads no record below (#2638, #1728).
func (u *userIndex) SizelessUIDs(folderID uint64) ([]uint32, error) {
	var out []uint32
	err := u.withFolderHeaderUnlocked(folderID, func(fs *folderState) error {
		// A folder whose records carry no size field has an aggregate that
		// vouches for none of them (#1752).
		from := fs.vsize.HighestUID
		if findExt(fs.file.Extensions, extNameVsize) == nil {
			from = 0
		}
		var err error
		out, err = fs.sizelessAbove(from)
		return err
	})
	return out, err
}

// sizelessAbove answers from the chunks above uid and the appends still
// pending; any candidate is confirmed on the records with pending applied,
// which a sizeless record is rare enough to pay for.
func (fs *folderState) sizelessAbove(uid uint32) ([]uint32, error) {
	if fs.isView {
		fs.pendMu.Lock()
		defer fs.pendMu.Unlock()
	}
	if fs.recs == nil {
		return nil, nil
	}
	if fs.recs.AllLoaded() || fs.isView && fs.applied.Load() || len(fs.pending) == 0 && fs.countersUnknown == 0 {
		return fs.sizelessIn(uid)
	}
	candidate, ok, err := fs.sizelessCandidate(uid)
	if err != nil || ok && !candidate {
		return nil, err
	}
	if err := fs.applyPendingLocked(); err != nil {
		return nil, err
	}
	return fs.sizelessIn(uid)
}

// sizelessIn scans the records above uid, loading their chunks.
func (fs *folderState) sizelessIn(uid uint32) ([]uint32, error) {
	seq, ok, err := fs.firstSeqAbove(uid)
	if err != nil {
		return nil, err
	}
	if !ok {
		if err := fs.loadRecords(); err != nil {
			return nil, err
		}
		seq = sort.Search(fs.recordCount(), func(i int) bool { return fs.recUID(i) > uid })
	}
	n := fs.recordCount()
	if err := fs.recs.Load(seq, n); err != nil {
		return nil, err
	}
	var out []uint32
	for i := seq; i < n; i++ {
		if v, _ := fs.recExt(i, extNameVsize); decodeVsizeRec(v) == 0 {
			out = append(out, fs.recUID(i))
		}
	}
	return out, nil
}

// sizelessCandidate says whether a base record above uid or a pending append
// carries no size; ok is false when the chunks cannot be read one by one.
func (fs *folderState) sizelessCandidate(uid uint32) (candidate, ok bool, err error) {
	seq, ok, err := fs.firstSeqAbove(uid)
	if err != nil || !ok {
		return false, ok, err
	}
	n := fs.recordCount()
	if err := fs.recs.Load(seq, n); err != nil {
		return false, false, err
	}
	for i := seq; i < n; i++ {
		if v, _ := fs.recExt(i, extNameVsize); decodeVsizeRec(v) == 0 {
			return true, true, nil
		}
	}
	layout := fs.recs.Layout()
	id, has := layout.ExtID(extNameVsize)
	stride := int(layout.RecordSize)
	for _, rec := range fs.pending {
		hdr, herr := mailindex.DecodeTxHeader(rec)
		if herr != nil {
			return false, true, herr
		}
		switch hdr.Type.Kind() {
		case mailindex.TxTypeAppend:
		case mailindex.TxTypeExtRecUpdate, mailindex.TxTypeExtIntro, mailindex.TxTypeExpungeGUID | mailindex.TxType(mailindex.TxExpungeProt):
			// A size stamped, or a record gone, after the append: the applied
			// records answer.
			return true, true, nil
		default:
			continue
		}
		if !has || stride == 0 {
			return true, true, nil
		}
		e := layout.Extensions[id]
		payload := rec[8:hdr.Size]
		for i := 0; i+stride <= len(payload); i += stride {
			if binary.LittleEndian.Uint32(payload[i:]) > uid &&
				decodeVsizeRec(payload[i+int(e.RecordOffset):i+int(e.RecordOffset+e.RecordSize)]) == 0 {
				return true, true, nil
			}
		}
	}
	return false, true, nil
}

// firstSeqAbove is the first record with a uid above uid, by a binary search
// that loads only the chunks it lands in; ok is false when the records were
// changed in memory and cannot be loaded by chunk.
func (fs *folderState) firstSeqAbove(uid uint32) (int, bool, error) {
	r := fs.recs
	// At or past what the base holds: no base record lies above, and none is
	// read to say so.
	if fs.baseNextUID != 0 && uid+1 >= fs.baseNextUID && r.Chunks() > 0 && !r.AllLoaded() {
		return r.Len(), true, nil
	}
	var lerr error
	ok := true
	seq := sort.Search(r.Len(), func(i int) bool {
		if lerr != nil || !ok {
			return true
		}
		ci := i / mailindex.ChunkRecords
		if !r.Loaded(ci) {
			if lerr = r.Load(i, i+1); lerr != nil {
				return true
			}
			if !r.Loaded(ci) {
				ok = false
				return true
			}
		}
		return r.UID(i) > uid
	})
	return seq, ok, lerr
}

// applyPendingLocked brings the records in with what is pending applied: a
// view's onto records of its own (ensureRecords, under pendMu held here), a
// writer's in place under fs.mu.
func (fs *folderState) applyPendingLocked() error {
	if !fs.isView {
		return fs.loadWriterRecords()
	}
	tmp := fs.siblingLocked()
	tmp.isView = false
	tmp.file = cloneIndexFile(fs.file)
	if err := tmp.loadWriterRecords(); err != nil {
		return err
	}
	fs.recs = tmp.recs
	fs.applied.Store(true)
	return nil
}

var _ mailbox.SizelessLister = (*userIndex)(nil)
