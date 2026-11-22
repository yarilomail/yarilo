package mdboxmap

import (
	"fmt"
	"sort"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
)

// Format v1 is the mailindex-backed base, kept because the format on disk decides
// what a rolled-back binary can read. It replays from the start, and a replayed
// refcount delta does not dedupe.

// readBaseV1Locked loads a v1 base into the record area.
func (m *Map) readBaseV1Locked() error {
	f, recs, err := mailindex.OpenLazy(m.path)
	if err != nil {
		return fmt.Errorf("mdboxmap/load v1: %w", err)
	}
	if err := recs.LoadAll(); err != nil {
		return fmt.Errorf("mdboxmap/load v1: %w", err)
	}
	layout := recs.Layout()
	entries := make([]MapEntry, 0, recs.Len())
	var maxUID, maxFileID uint32
	for i := 0; i < recs.Len(); i++ {
		e, derr := decodeEntry(layout, recs.Raw(i))
		if derr != nil {
			return fmt.Errorf("mdboxmap/load v1: uid=%d: %w", recs.UID(i), derr)
		}
		entries = append(entries, e)
		if e.UID > maxUID {
			maxUID = e.UID
		}
		if e.FileID > maxFileID {
			maxFileID = e.FileID
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].UID < entries[j].UID })

	m.st.base.release()
	m.st = store{base: memBase(entries, ^uint32(0)), count: len(entries)}
	m.loaded = true
	m.onDisk = FormatV1

	m.indexID = f.Header.IndexID
	m.nextMapUID = f.Header.NextUID
	if maxUID+1 > m.nextMapUID {
		m.nextMapUID = maxUID + 1
	}
	if m.nextMapUID == 0 {
		m.nextMapUID = 1
	}
	m.highestFileID, m.rebuildCount, m.createFileID, m.createTime = 0, 0, 0, 0
	if ext := findExt(f.Extensions, extMap); ext != nil {
		m.highestFileID, m.rebuildCount, m.createFileID, m.createTime = decodeMapHeader(ext.HdrData)
	}
	if maxFileID > m.highestFileID {
		m.highestFileID = maxFileID
	}
	// v1 has nowhere to record which log it folded, so a reader can only replay
	// from the start.
	m.lineage, m.foldedLineage, m.foldedOffset = 0, 0, 0
	return nil
}

// writeBaseV1Locked rewrites the whole base in v1. mailindex.Recreate is
// atomic (.tmp + rename), the same guarantee the v2 writer gives.
func (m *Map) writeBaseV1Locked(keep func(*MapEntry) bool) error {
	f, err := mailindex.NewFile(m.indexID, defaultExtensions(m.highestFileID))
	if err != nil {
		return fmt.Errorf("mdboxmap/write v1: NewFile: %w", err)
	}
	sn := m.st.snapshot()
	defer sn.release()
	var kept []MapEntry
	if err := sn.each(func(e MapEntry) error {
		if keep != nil && !keep(&e) {
			return nil
		}
		kept = append(kept, e)
		return nil
	}); err != nil {
		return fmt.Errorf("mdboxmap/write v1: %w", err)
	}
	ext := findExt(f.Extensions, extMap)
	if ext == nil {
		return fmt.Errorf("mdboxmap/write v1: missing %q extension", extMap)
	}
	ext.HdrData = encodeMapHeader(m.highestFileID, m.rebuildCount, m.createFileID, m.createTime)
	ext.HdrSize = uint32(len(ext.HdrData))
	layout, err := mailindex.ComputeRecordLayout(f.Extensions)
	if err != nil {
		return fmt.Errorf("mdboxmap/write v1: layout: %w", err)
	}
	extBytes, err := mailindex.EncodeExtHeaders(layout.Extensions)
	if err != nil {
		return fmt.Errorf("mdboxmap/write v1: encode ext headers: %w", err)
	}
	f.Extensions = layout.Extensions
	f.Layout = layout
	f.Header.RecordSize = layout.RecordSize
	f.Header.HeaderSize = uint32(mailindex.HeaderMinSize) + uint32(len(extBytes))
	f.Header.NextUID = m.nextMapUID
	f.Header.MessagesCount = uint32(len(kept))
	in := f.ToRecreateInput(m.path)
	in.RecordBytes = make([]byte, 0, len(kept)*int(layout.RecordSize))
	for _, e := range kept {
		rec, err := encodeEntry(layout, e)
		if err != nil {
			return fmt.Errorf("mdboxmap/write v1: %w", err)
		}
		in.RecordBytes = append(in.RecordBytes, rec...)
	}
	if _, err := mailindex.Recreate(in); err != nil {
		return fmt.Errorf("mdboxmap/write v1: %w", err)
	}
	m.onDisk = FormatV1
	m.st.base.release()
	m.st = store{base: memBase(kept, ^uint32(0)), count: len(kept)}
	return nil
}

// findExt returns a pointer to the named extension in the slice, or nil if not
// found.
func findExt(exts []mailindex.Extension, name string) *mailindex.Extension {
	for i := range exts {
		if exts[i].Name == name {
			return &exts[i]
		}
	}
	return nil
}
