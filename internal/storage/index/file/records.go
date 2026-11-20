package file

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
)

// A folder state holds its records as chunks (recs), lazy on a view of the
// base alone, loaded on a writer (INDEX-MEMORY.md §3).

func (fs *folderState) recordCount() int {
	if fs.recs == nil {
		return 0 // a handle that left its records on disk
	}
	return fs.recs.Len()
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

func (fs *folderState) recUID(i int) uint32 { return fs.recs.UID(i) }

func (fs *folderState) recFlags(i int) mailindex.MailFlag { return fs.recs.Flags(i) }

// recExt is the extension's bytes in record i, ok when the layout carries it:
// the chunk's bytes, copied by a caller keeping them.
func (fs *folderState) recExt(i int, name string) ([]byte, bool) {
	id, ok := fs.recs.Layout().ExtID(name)
	if !ok {
		return nil, false
	}
	return fs.recs.Ext(i, id), true
}

// forkRecords gives a view private header data and records a replay may
// write: chunks forked from the shared ones.
func (fs *folderState) forkRecords() {
	fs.file = cloneIndexFile(fs.file)
	if fs.recs == nil {
		return
	}
	// Unloaded chunks stay so: the replay defers what changes them.
	fs.recs = fs.recs.Fork()
}

// ensureRecords gives a view its records with pending applied, loading them;
// a view's read of recs follows it. A no-op for a writer, which holds fs.mu.
func (fs *folderState) ensureRecords() error {
	if !fs.isView || len(fs.pending) == 0 && fs.countersUnknown == 0 {
		return nil
	}
	fs.pendMu.Lock()
	defer fs.pendMu.Unlock()
	if fs.applied.Load() {
		return nil
	}
	// On a copy: header readers read this view's header meanwhile.
	return fs.applyPendingLocked()
}

// recsDeferred says chunks are left on disk with what changes them pending,
// without reading a view's recs, which ensureRecords may be replacing.
func (fs *folderState) recsDeferred() bool {
	if fs.isView {
		return (len(fs.pending) > 0 || fs.countersUnknown != 0) && !fs.applied.Load()
	}
	return fs.recs != nil && !fs.recs.AllLoaded()
}

// The write half, for the one journal replay (INDEX-MEMORY.md §4): decoded
// records are changed as before, a view's records through the structure, which
// copies a chunk only while a snapshot shares it.

// recSeqOfUID finds a uid by search in the chunks' uid order.
func (fs *folderState) recSeqOfUID(uid uint32) (int, bool) {
	if fs.recs == nil {
		return 0, false
	}
	return fs.recs.SeqOfUID(uid)
}

// recEachInRange calls fn for every record with a uid in [uid1, uid2].
func (fs *folderState) recEachInRange(uid1, uid2 uint32, fn func(seq int)) {
	seq, _ := fs.recSeqOfUID(uid1)
	for ; seq < fs.recordCount() && fs.recs.UID(seq) <= uid2; seq++ {
		fn(seq)
	}
}

func (fs *folderState) recSetFlags(seq int, f mailindex.MailFlag) { fs.recs.SetFlags(seq, f) }

// recSetExt writes an extension the layout carries.
func (fs *folderState) recSetExt(seq int, name string, b []byte) {
	if id, ok := fs.recs.Layout().ExtID(name); ok {
		fs.recs.SetExt(seq, id, b)
	}
}

// recAppend adds a record, raw in the current layout. Records keep uid order,
// which the searches stand on: an append below the last uid goes to its place.
func (fs *folderState) recAppend(uid uint32, raw []byte) {
	if n := fs.recs.Len(); n > 0 && uid < fs.recs.UID(n-1) {
		seq, _ := fs.recs.SeqOfUID(uid)
		fs.recs.Insert(seq, raw)
		return
	}
	fs.recs.Append(raw)
}

func (fs *folderState) recExpunge(seq int) { fs.recs.Expunge(seq) }

func sameLayout(a, b mailindex.RecordLayout) bool {
	if a.RecordSize != b.RecordSize {
		return false
	}
	// Only the extensions with bytes in a record place them; header-only ones
	// sit at offset 0 in no fixed order.
	type place struct{ off, size uint16 }
	of := func(l mailindex.RecordLayout) map[string]place {
		m := map[string]place{}
		for _, e := range l.Extensions {
			if e.RecordSize != 0 {
				m[e.Name] = place{e.RecordOffset, e.RecordSize}
			}
		}
		return m
	}
	pa, pb := of(a), of(b)
	if len(pa) != len(pb) {
		return false
	}
	for name, p := range pa {
		if pb[name] != p {
			return false
		}
	}
	return true
}

// openWriterBase loads a base for the writer: a fork of the base image the
// readers share, every chunk in memory, so the base's records are held once
// however many handles and views read it (INDEX-MEMORY.md §4).
func openWriterBase(fs *folderState) (*mailindex.File, *mailindex.Records, error) {
	img, err := imageFor(fs)
	if err != nil {
		return nil, nil, err
	}
	return cloneIndexFile(img.file), img.recs.Fork(), nil
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
	if fs.recs != nil && !fs.recs.InUIDOrder() {
		fs.recs = fs.recs.SortedByUID()
	}
}

// nowUnix is the clock the index stamps its files with; a test of their bytes
// fixes it.
var nowUnix = func() int64 { return time.Now().Unix() }

// recordBuilder writes one record's fields straight into its bytes in the
// folder's layout: what a base is built from, without a decoded record.
type recordBuilder struct {
	layout mailindex.RecordLayout
	raw    []byte
}

func (fs *folderState) newRecord(uid uint32, flags mailindex.MailFlag) (recordBuilder, error) {
	layout, err := fs.recordLayout()
	if err != nil {
		return recordBuilder{}, err
	}
	return newRecordIn(layout, uid, flags), nil
}

// recordLayout is the layout the state's extensions describe: computed once
// for a batch of records, not once a record.
func (fs *folderState) recordLayout() (mailindex.RecordLayout, error) {
	layout, err := mailindex.ComputeRecordLayout(fs.file.Extensions)
	if err != nil {
		return mailindex.RecordLayout{}, fmt.Errorf("fileindex/records: layout: %w", err)
	}
	return layout, nil
}

func newRecordIn(layout mailindex.RecordLayout, uid uint32, flags mailindex.MailFlag) recordBuilder {
	b := recordBuilder{layout: layout, raw: make([]byte, layout.RecordSize)}
	binary.LittleEndian.PutUint32(b.raw, uid)
	b.raw[4] = byte(flags)
	return b
}

// set writes an extension's bytes; one the layout does not carry is dropped,
// as the encoder of decoded records dropped it.
func (b recordBuilder) set(name string, v []byte) {
	id, ok := b.layout.ExtID(name)
	if !ok {
		return
	}
	e := b.layout.Extensions[id]
	copy(b.raw[e.RecordOffset:e.RecordOffset+e.RecordSize], v)
}

// replaceRecords makes raws, each a record in the folder's layout, the
// state's records: a base being built.
func (fs *folderState) replaceRecords(raws [][]byte) error {
	layout, err := mailindex.ComputeRecordLayout(fs.file.Extensions)
	if err != nil {
		return fmt.Errorf("fileindex/records: layout: %w", err)
	}
	all := make([]byte, 0, len(raws)*int(layout.RecordSize))
	for _, r := range raws {
		all = append(all, r...)
	}
	recs, err := mailindex.ChunksFromBytes(all, layout)
	if err != nil {
		return fmt.Errorf("fileindex/records: %w", err)
	}
	fs.recs = recs
	return nil
}

// reencodeRecords puts the state's chunks in the layout its extensions now
// describe: a record extension added or resized, once for every record, as
// the reference's resize (INDEX-MEMORY.md §4).
func (fs *folderState) reencodeRecords() error {
	if fs.recs == nil {
		return nil
	}
	layout, err := mailindex.ComputeRecordLayout(fs.file.Extensions)
	if err != nil {
		return fmt.Errorf("fileindex/records: layout: %w", err)
	}
	if sameLayout(layout, fs.recs.Layout()) {
		return nil
	}
	if err := fs.recs.LoadAll(); err != nil {
		return fmt.Errorf("fileindex/records: %w", err)
	}
	fs.recs = fs.recs.Reencode(layout)
	return nil
}

// absoluteCounters journals the header's message, seen and deleted counts as
// they stand: every flag change and expunge carries them, so a replay that
// defers the change to a chunk not loaded keeps the counters right
// (INDEX-MEMORY.md §4, reviewer 2026-11-20).
func (fs *folderState) absoluteCounters() [][]byte {
	h := fs.file.Header
	return [][]byte{encU32Update(32, h.MessagesCount), encU32Update(40, h.SeenMessagesCount), encU32Update(44, h.DeletedMessagesCount)}
}

// The counters a change to a record not loaded moved without telling.
const (
	counterMessages uint8 = 1 << iota
	counterSeen
	counterDeleted
)

// deferRecord keeps a journal record that changes records the writer has not
// loaded in pending, in journal order, for the one replay to apply when the
// chunks load (INDEX-MEMORY.md §4). Header records apply at once. A record
// that needs the chunks now (a new record width, an extension reset) loads
// them instead.
func (fs *folderState) deferRecord(kind mailindex.TxType, payload []byte, st *replayState) (bool, error) {
	if fs.recs == nil || fs.loading || fs.recs.AllLoaded() {
		return false, nil
	}
	var unknown uint8
	switch kind {
	case mailindex.TxTypeFlagUpdate:
		unknown = counterSeen | counterDeleted
	case mailindex.TxTypeExpungeGUID | mailindex.TxType(mailindex.TxExpungeProt):
		unknown = counterMessages | counterSeen | counterDeleted
		fs.pendingExpunge = true
	case mailindex.TxTypeAppend:
		unknown = counterMessages | counterSeen | counterDeleted
		if stride := int(st.layout.RecordSize); stride > 0 {
			for i := 0; i+stride <= len(payload); i += stride {
				if uid := binary.LittleEndian.Uint32(payload[i:]); uid != 0 {
					if fs.pendingUIDs == nil {
						fs.pendingUIDs = map[uint32]struct{}{}
					}
					fs.pendingUIDs[uid] = struct{}{}
				}
			}
		}
	case mailindex.TxTypeModseqUpdate:
		// The header's highest modseq moves now; the records' at load.
		for i := 0; i+12 <= len(payload); i += 12 {
			ms := uint64(binary.LittleEndian.Uint32(payload[i+4:])) | uint64(binary.LittleEndian.Uint32(payload[i+8:]))<<32
			st.maxModseq = max(st.maxModseq, ms)
		}
	case mailindex.TxTypeKeywordUpdate, mailindex.TxTypeKeywordReset, mailindex.TxTypeExtRecUpdate:
	case mailindex.TxTypeExtIntro:
		intro, ok := mailindex.DecodeTxExtIntroPayload(payload)
		if !ok || intro.ExtID == mailindex.TxExtIntroNewExt {
			// A new extension changes the record width: the chunks load.
			return false, fs.loadWriterRecords()
		}
		// It names the extension the updates after it write: kept with them,
		// and applied now for the context.
		fs.pending = append(fs.pending, encLogRec(kind, 0, payload))
		return false, nil
	case mailindex.TxTypeExtReset:
		return false, fs.loadWriterRecords()
	default:
		return false, nil
	}
	if kind == mailindex.TxTypeKeywordUpdate {
		// The registry grows from the record's name now; the bits at load.
		rec, ok := mailindex.DecodeTxKeywordUpdatePayload(payload)
		if ok {
			if _, reg, err := keywordsBitmaskFor(fs.keywords, []string{rec.Name}); err == nil {
				fs.keywords = reg
				if err := fs.persistKeywordRegistry(); err != nil {
					return true, err
				}
			}
		}
	}
	fs.pending = append(fs.pending, encLogRec(kind, 0, payload))
	fs.countersUnknown |= unknown
	return true, nil
}

// applyPending applies the deferred records to the chunks now loaded, by the
// one replay, and recounts the header from the records: applying a flag change
// again moves a counter the absolute updates already moved.
func (fs *folderState) applyPending() error {
	if len(fs.pending) == 0 && fs.countersUnknown == 0 {
		return nil
	}
	layout, err := mailindex.ComputeRecordLayout(fs.file.Extensions)
	if err != nil {
		return fmt.Errorf("fileindex/pending: layout: %w", err)
	}
	st := replayState{layout: layout}
	saved := fs.file.Header
	for _, rec := range fs.pending {
		hdr, herr := mailindex.DecodeTxHeader(rec)
		if herr != nil {
			return fmt.Errorf("fileindex/pending: %w", herr)
		}
		if err := fs.applyRecord(hdr.Type.Kind(), rec[8:hdr.Size], &st, -1); err != nil {
			return err
		}
	}
	// What only the header knows stays as the journal left it.
	fs.file.Header.NextUID = max(fs.file.Header.NextUID, saved.NextUID)
	fs.file.Header.FirstRecentUID = saved.FirstRecentUID
	fs.pending, fs.pendingUIDs, fs.countersUnknown, fs.pendingExpunge = nil, nil, 0, false
	fs.reconcileHeaderLocked()
	// What a catch-up took off for pending expunges is counted again whole.
	fs.recalcVsizeLocked()
	return nil
}

// newBaseLoaded starts a base's pending afresh: the uid no base record reaches
// is its next uid when it is marked as in order, and none known otherwise.
func (fs *folderState) newBaseLoaded(mf *mailindex.File) {
	fs.pending, fs.pendingUIDs, fs.countersUnknown, fs.baseNextUID, fs.pendingExpunge = nil, nil, 0, 0, false
	if readLineage(mf).Flags&lineageInUIDOrder != 0 {
		fs.baseNextUID = mf.Header.NextUID
	}
}

// uidTaken answers whether uid is in the folder, loading no chunk for a uid
// above every base record and not appended since: what a delivery allocated.
func (fs *folderState) uidTaken(uid uint32) (bool, error) {
	if fs.recs == nil {
		return false, nil
	}
	if !fs.recs.AllLoaded() {
		if _, appended := fs.pendingUIDs[uid]; !appended && fs.baseNextUID != 0 && uid >= fs.baseNextUID {
			return false, nil
		}
		if err := fs.loadWriterRecords(); err != nil {
			return false, err
		}
	}
	_, taken := fs.recs.SeqOfUID(uid)
	return taken, nil
}
