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
	layout, err := mailindex.ComputeRecordLayout(fs.file.Extensions)
	if err != nil {
		return recordBuilder{}, fmt.Errorf("fileindex/records: layout: %w", err)
	}
	b := recordBuilder{layout: layout, raw: make([]byte, layout.RecordSize)}
	binary.LittleEndian.PutUint32(b.raw, uid)
	b.raw[4] = byte(flags)
	return b, nil
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
