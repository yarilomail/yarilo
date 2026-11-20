package file

import (
	"fmt"
	"log/slog"
	"sort"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
)

// A view with no log tail holds the base's lazy chunks (recs), every other
// folderState file.Records; the accessors read either (INDEX-MEMORY.md §3).

func (fs *folderState) recordCount() int {
	if fs.recs != nil {
		return fs.recs.Len()
	}
	return len(fs.file.Records)
}

// loadRecords brings a lazy view's chunks into memory: one open of the base.
// A base replaced since the view was built is mailindex.ErrBaseMoved.
func (fs *folderState) loadRecords() error {
	if fs.recs == nil {
		return nil
	}
	if err := fs.recs.LoadAll(); err != nil {
		return fmt.Errorf("fileindex/records: %w", err)
	}
	return nil
}

func (fs *folderState) recUID(i int) uint32 {
	if fs.recs != nil {
		return fs.recs.UID(i)
	}
	return fs.file.Records[i].UID
}

func (fs *folderState) recFlags(i int) mailindex.MailFlag {
	if fs.recs != nil {
		return fs.recs.Flags(i)
	}
	return fs.file.Records[i].Flags
}

// recExt is the extension's bytes in record i, ok when the layout carries it.
// From a lazy view the bytes are the chunk's: copied by a caller keeping them.
func (fs *folderState) recExt(i int, name string) ([]byte, bool) {
	if fs.recs != nil {
		id, ok := fs.recs.Layout().ExtID(name)
		if !ok {
			return nil, false
		}
		return fs.recs.Ext(i, id), true
	}
	v, ok := fs.file.Records[i].Ext[name]
	return v, ok
}

// forkRecords gives a view private header data and records a replay may
// write: chunks forked from the shared ones, or decoded records copied.
func (fs *folderState) forkRecords() error {
	fs.file = cloneIndexFile(fs.file)
	if fs.recs == nil {
		return nil
	}
	if err := fs.recs.LoadAll(); err != nil {
		return fmt.Errorf("fileindex/records: %w", err)
	}
	fs.recs = fs.recs.Fork()
	return nil
}

// The write half, for the one journal replay (INDEX-MEMORY.md §4): decoded
// records are changed as before, a view's records through the structure, which
// copies a chunk only while a snapshot shares it.

// recSeqOfUID finds a uid: by search in the chunks, which hold the base's uid
// order; by scan in decoded records, whose order no writer promises.
func (fs *folderState) recSeqOfUID(uid uint32) (int, bool) {
	if fs.recs != nil {
		return fs.recs.SeqOfUID(uid)
	}
	for i, rec := range fs.file.Records {
		if rec.UID == uid {
			return i, true
		}
	}
	return 0, false
}

// recEachInRange calls fn for every record with a uid in [uid1, uid2].
func (fs *folderState) recEachInRange(uid1, uid2 uint32, fn func(seq int)) {
	if fs.recs != nil {
		seq, _ := fs.recs.SeqOfUID(uid1)
		for ; seq < fs.recs.Len() && fs.recs.UID(seq) <= uid2; seq++ {
			fn(seq)
		}
		return
	}
	for i, rec := range fs.file.Records {
		if rec.UID >= uid1 && rec.UID <= uid2 {
			fn(i)
		}
	}
}

func (fs *folderState) recSetFlags(seq int, f mailindex.MailFlag) {
	if fs.recs != nil {
		fs.recs.SetFlags(seq, f)
		return
	}
	fs.file.Records[seq].Flags = f
}

// recSetExt writes an extension the layout carries; in decoded records any.
func (fs *folderState) recSetExt(seq int, name string, b []byte) {
	if fs.recs != nil {
		if id, ok := fs.recs.Layout().ExtID(name); ok {
			fs.recs.SetExt(seq, id, b)
		}
		return
	}
	rec := fs.file.Records[seq]
	if rec.Ext == nil {
		rec.Ext = make(map[string][]byte)
	}
	rec.Ext[name] = b
}

// recAppend adds rec, whose bytes in the current layout are raw.
func (fs *folderState) recAppend(rec *mailindex.Record, raw []byte) {
	if fs.recs != nil {
		// Records keep uid order, which the searches stand on: an append
		// below the last uid goes to its place.
		if n := fs.recs.Len(); n > 0 && rec.UID < fs.recs.UID(n-1) {
			seq, _ := fs.recs.SeqOfUID(rec.UID)
			fs.recs.Insert(seq, raw)
			return
		}
		fs.recs.Append(raw)
		return
	}
	fs.file.Records = append(fs.file.Records, rec)
}

func (fs *folderState) recExpunge(seq int) {
	if fs.recs != nil {
		fs.recs.Expunge(seq)
		return
	}
	fs.file.Records = append(fs.file.Records[:seq], fs.file.Records[seq+1:]...)
}

// decodeRecords leaves the chunks for decoded records: what a record width the
// structure does not have takes, until the next base.
func (fs *folderState) decodeRecords() error {
	recs, err := fs.recs.Decode()
	if err != nil {
		return fmt.Errorf("fileindex/records: %w", err)
	}
	fs.file.Records, fs.recs = recs, nil
	return nil
}

func sameLayout(a, b mailindex.RecordLayout) bool {
	if a.RecordSize != b.RecordSize || len(a.Extensions) != len(b.Extensions) {
		return false
	}
	for i := range a.Extensions {
		if a.Extensions[i].Name != b.Extensions[i].Name || a.Extensions[i].RecordOffset != b.Extensions[i].RecordOffset ||
			a.Extensions[i].RecordSize != b.Extensions[i].RecordSize {
			return false
		}
	}
	return true
}

// writerDecoded is a test seam: the writer holds decoded records, as before
// chunks, so a row can compare the two. False everywhere else.
var writerDecoded bool

// openWriterBase loads a base for the writer: a fork of the base image the
// readers share, every chunk in memory, so the base's records are held once
// however many handles and views read it (INDEX-MEMORY.md §4).
func openWriterBase(fs *folderState) (*mailindex.File, *mailindex.Records, error) {
	if writerDecoded {
		f, err := mailindex.Open(fs.indexPath)
		return f, nil, err
	}
	img, err := imageFor(fs)
	if err != nil {
		return nil, nil, err
	}
	if img.recs == nil {
		return cloneIndexFile(img.file), nil, nil
	}
	return cloneIndexFile(img.file), img.recs.Fork(), nil
}

// decodeForRewrite gives an operation that still writes decoded records
// those records, until step 3b-2b moves it onto chunks. A layout that does not
// decode is a damaged index: an error for the caller, not the process's end.
func (fs *folderState) decodeForRewrite() error {
	if fs.recs == nil {
		return nil
	}
	if err := fs.decodeRecords(); err != nil {
		slog.Error("fileindex: the writer's records do not decode", "folder", fs.folder, "err", err)
		return err
	}
	return nil
}

// extIntroRecord names a known extension by its place and generation, for the
// records that follow it in the journal.
func (fs *folderState) extIntroRecord(name string) ([]byte, error) {
	for i := range fs.file.Extensions {
		ext := fs.file.Extensions[i]
		if ext.Name == name {
			return encLogRec(mailindex.TxTypeExtIntro, 0, mailindex.EncodeTxExtIntroPayload(mailindex.TxExtIntro{
				ExtID: uint32(i), ResetID: ext.ResetID, HdrSize: ext.HdrSize,
				RecordSize: ext.RecordSize, RecordAlign: ext.RecordAlign,
			})), nil
		}
	}
	return nil, fmt.Errorf("fileindex/records: no extension %q", name)
}

// extRecRecords journals per-record updates of one extension: the intro that
// names it by its place and generation, then the updates, sorted by uid.
func (fs *folderState) extRecRecords(name string, upds []mailindex.TxExtRecUpdate) ([][]byte, error) {
	if len(upds) == 0 {
		return nil, nil
	}
	ext := findExt(fs.file.Extensions, name)
	if ext == nil || ext.RecordSize == 0 {
		return nil, fmt.Errorf("fileindex/records: no record extension %q", name)
	}
	intro, err := fs.extIntroRecord(name)
	if err != nil {
		return nil, err
	}
	sort.Slice(upds, func(i, j int) bool { return upds[i].UID < upds[j].UID })
	return [][]byte{intro,
		encLogRec(mailindex.TxTypeExtRecUpdate, 0, mailindex.EncodeTxExtRecUpdatePayload(int(ext.RecordSize), upds)),
	}, nil
}

// journalRecords applies records through the replay and appends them: a
// writer's change that reaches memory and the journal the same way.
func (fs *folderState) journalRecords(recs [][]byte) error {
	if len(recs) == 0 {
		return nil
	}
	if err := fs.applyRecords(recs); err != nil {
		return err
	}
	return fs.appendMutLog(recs...)
}

// orderImage makes the records of a base not marked as in uid order ordered in
// memory before anything searches them: such a base is read whole, and the
// next base write puts it on disk in order, marked (#2632).
func orderImage(img *baseImage, path string) error {
	if img.lineage.Flags&lineageInUIDOrder != 0 {
		return nil
	}
	if img.recs == nil {
		sort.SliceStable(img.file.Records, func(i, j int) bool { return img.file.Records[i].UID < img.file.Records[j].UID })
		return nil
	}
	if err := img.recs.LoadAll(); err != nil {
		return fmt.Errorf("fileindex/order: %w", err)
	}
	if !img.recs.InUIDOrder() {
		slog.Info("fileindex: a base's records are out of uid order; ordered in memory until the next base write",
			"index", path, "records", img.recs.Len())
		img.recs = img.recs.SortedByUID()
	}
	return nil
}

// orderRecords puts the state's records in uid order before a base write.
func (fs *folderState) orderRecords() {
	if fs.recs != nil {
		if !fs.recs.InUIDOrder() {
			fs.recs = fs.recs.SortedByUID()
		}
		return
	}
	sort.SliceStable(fs.file.Records, func(i, j int) bool { return fs.file.Records[i].UID < fs.file.Records[j].UID })
}
