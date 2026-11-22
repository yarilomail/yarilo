package mdboxmap

import (
	"fmt"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
)

// UpdateRefcounts applies a delta to every listed map_uid under one lock hop.
// A missing uid does not stop the rest -- the error names it -- so one stale
// record cannot leave a batch of bodies referenced forever (#1884).
func (m *Map) UpdateRefcounts(mapUIDs []uint32, delta int16) error {
	if len(mapUIDs) == 0 {
		return nil
	}
	return m.withMapLock(func() error {
		if err := m.reloadLocked(); err != nil {
			return err
		}
		deltas := make([]mailindex.TxExtAtomicInc, 0, len(mapUIDs))
		var missing []uint32
		for _, uid := range mapUIDs {
			e, ok, err := m.st.get(uid)
			if err != nil {
				m.invalidateLocked()
				return fmt.Errorf("mdboxmap/refcount: %w", err)
			}
			if !ok {
				missing = append(missing, uid)
				continue
			}
			e.RefCount = clampRef(int32(e.RefCount) + int32(delta))
			if err := m.st.put(e); err != nil {
				m.invalidateLocked()
				return fmt.Errorf("mdboxmap/refcount: %w", err)
			}
			deltas = append(deltas, mailindex.TxExtAtomicInc{UID: uid, Diff: int32(delta)})
		}
		if len(deltas) == 0 {
			// Nothing was changed in memory, so nothing to invalidate.
			return fmt.Errorf("mdboxmap/refcount: map_uid %v not found", missing)
		}
		// Appended, not rewritten: every save and delete changes a refcount, so
		// a base rewrite here priced one operation at a full file (#1205).
		if err := m.appendRefcountLogLocked(deltas); err != nil {
			// The records changed but the write did not happen, and nothing on
			// disk moved -- so the next reload fast-paths and keeps the phantom
			// value, which for a decrement is what the purge scan reads.
			m.invalidateLocked()
			return err
		}
		if len(missing) > 0 {
			return fmt.Errorf("mdboxmap/refcount: map_uid %v not found", missing)
		}
		return nil
	})
}

// SetRefcountsFromReferences rewrites every refcount from refs, zero for a record
// it does not name, so an unreferenced one is reclaimed rather than resurrected.
func (m *Map) SetRefcountsFromReferences(refs map[uint32]int) (int, error) {
	zeroed := 0
	err := m.withMapLock(func() error {
		if err := m.reloadLocked(); err != nil {
			return err
		}
		zeroed = 0
		return m.foldLocked(func(e *MapEntry) bool {
			n := refs[e.UID]
			if n < 0 {
				n = 0
			}
			if n > 0xFFFF {
				n = 0xFFFF
			}
			if n == 0 {
				zeroed++
			}
			e.RefCount = uint16(n)
			return true
		})
	})
	if err != nil {
		return 0, err
	}
	return zeroed, nil
}

// SetGUIDs stamps a GUID onto records carrying none, taking it from the folder
// index of a converted store rather than from a walk over every storage file. A
// record that already has one is left alone.
func (m *Map) SetGUIDs(guids map[uint32][16]byte) (int, error) {
	if len(guids) == 0 {
		return 0, nil
	}
	var zero [16]byte
	written := 0
	err := m.withMapLock(func() error {
		if err := m.reloadLocked(); err != nil {
			return err
		}
		written = 0
		stamp := func(e *MapEntry) bool {
			if g, ok := guids[e.UID]; ok && g != zero && e.GUID == zero {
				e.GUID = g
				written++
			}
			return true
		}
		if err := m.st.scan(func(e MapEntry) error { stamp(&e); return nil }); err != nil {
			return fmt.Errorf("mdboxmap/set-guids: %w", err)
		}
		if written == 0 {
			return nil
		}
		written = 0
		return m.foldLocked(stamp)
	})
	if err != nil {
		return 0, err
	}
	return written, nil
}

// GetZeroRefFiles returns the file_ids holding at least one zero-refcount
// record -- purge candidates, which the caller compacts through AppendMove.
func (m *Map) GetZeroRefFiles() ([]uint32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loaded {
		return nil, nil
	}
	// Refcounts live in the log until it is compacted, and this answer decides
	// what purge physically deletes: reading the base alone would offer a file
	// whose count was raised moments ago.
	if err := m.reloadLocked(); err != nil {
		return nil, fmt.Errorf("mdboxmap/zero-ref: %w", err)
	}
	seen := make(map[uint32]bool)
	if err := m.st.scan(func(e MapEntry) error {
		if e.RefCount == 0 {
			seen[e.FileID] = true
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("mdboxmap/zero-ref: %w", err)
	}
	out := make([]uint32, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out, nil
}

// RecordsInFile returns every entry living in one physical file, for purge to
// split into what it copies forward and what it expunges.
func (m *Map) RecordsInFile(fileID uint32) ([]MapEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loaded {
		return nil, nil
	}
	// Same reason as GetZeroRefFiles: purge copies forward what is referenced
	// and drops the rest, from these counts.
	if err := m.reloadLocked(); err != nil {
		return nil, fmt.Errorf("mdboxmap/records-in-file: %w", err)
	}
	return m.recordsInFileLocked(fileID)
}

func (m *Map) recordsInFileLocked(fileID uint32) ([]MapEntry, error) {
	var out []MapEntry
	err := m.st.scan(func(e MapEntry) error {
		if e.FileID == fileID {
			out = append(out, e)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("mdboxmap/records-in-file: %w", err)
	}
	return out, nil
}
