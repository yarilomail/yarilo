package mdboxmap

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// GUIDWalks counts the walks a GUID lookup costs, so "no walk" is a number a
// test reads rather than a claim (#1700).
var guidWalks atomic.Int64

// GUIDWalks returns the count, and ResetGUIDWalks zeroes it.
func GUIDWalks() int  { return int(guidWalks.Load()) }
func ResetGUIDWalks() { guidWalks.Store(0) }

// lockRead takes the in-process map mutex and records the wait. A read needs no
// cross-process lock, but it queues behind a writer that is waiting for one --
// which is the whole question in #1205, and unanswerable without this number.
func lockRead(mu *sync.Mutex) {
	start := time.Now()
	mu.Lock()
	metricMapReadBlocked.Observe(time.Since(start).Seconds())
}

// Lookup resolves one map_uid to its location and refcount, under m.mu only: a
// sibling's concurrent append can leave the view briefly stale, never torn,
// since the base is replaced atomically.
func (m *Map) Lookup(mapUID uint32) (MapEntry, bool, error) {
	lockRead(&m.mu)
	defer m.mu.Unlock()
	var e MapEntry
	var ok bool
	if err := m.retryBaseLocked(func() (err error) {
		e, ok, err = m.lookupLocked(mapUID)
		return err
	}); err != nil || ok {
		return e, ok, err
	}
	// Miss: a sibling may have appended it since; refresh and retry.
	if rerr := m.reloadLocked(); rerr != nil {
		return MapEntry{}, false, rerr
	}
	err := m.retryBaseLocked(func() (err error) {
		e, ok, err = m.lookupLocked(mapUID)
		return err
	})
	return e, ok, err
}

// retryBaseLocked runs fn again on a reloaded base when a read of the one it
// had failed: another node's fold replaces the file, and on NFS the old one
// answers ESTALE. A second failure is the caller's.
func (m *Map) retryBaseLocked(fn func() error) error {
	err := fn()
	if !errors.Is(err, errBaseRead) {
		return err
	}
	m.invalidateLocked()
	if rerr := m.reloadLocked(); rerr != nil {
		return rerr
	}
	return fn()
}

// LookupReloaded is Lookup on a freshly replayed view, for a reader whose file
// vanished: a hit in a stale view can still name a file a purge removed.
func (m *Map) LookupReloaded(mapUID uint32) (MapEntry, bool, error) {
	lockRead(&m.mu)
	defer m.mu.Unlock()
	if err := m.reloadLocked(); err != nil {
		return MapEntry{}, false, err
	}
	var e MapEntry
	var ok bool
	err := m.retryBaseLocked(func() (err error) {
		e, ok, err = m.lookupLocked(mapUID)
		return err
	})
	return e, ok, err
}

func (m *Map) lookupLocked(mapUID uint32) (MapEntry, bool, error) {
	if !m.loaded {
		return MapEntry{}, false, nil
	}
	e, ok, err := m.st.get(mapUID)
	if err != nil {
		return MapEntry{}, false, fmt.Errorf("mdboxmap/lookup: %w", err)
	}
	return e, ok, nil
}

// EachRecord calls fn with every record in map order, from a view taken under
// the mutex and walked without it; fn may call the map. An error from fn stops
// the walk and is returned.
func (m *Map) EachRecord(fn func(MapEntry) error) error {
	lockRead(&m.mu)
	sn := m.st.snapshot()
	m.mu.Unlock()
	noteFullScan()
	defer sn.release()
	return sn.each(fn)
}

// EachRecordReloaded is EachRecord after a reload, for a caller pairing the
// map with what is on disk now.
func (m *Map) EachRecordReloaded(fn func(MapEntry) error) error {
	m.mu.Lock()
	if err := m.reloadLocked(); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("mdboxmap/records: %w", err)
	}
	sn := m.st.snapshot()
	m.mu.Unlock()
	noteFullScan()
	defer sn.release()
	return sn.each(fn)
}

// Records collects EachRecord: the whole map in the heap, for tests and
// tools, never a path a session runs.
func (m *Map) Records() []MapEntry {
	var out []MapEntry
	_ = m.EachRecord(func(e MapEntry) error { out = append(out, e); return nil })
	return out
}

// RecordsReloaded collects EachRecordReloaded, under the same terms as Records.
func (m *Map) RecordsReloaded() ([]MapEntry, error) {
	var out []MapEntry
	err := m.EachRecordReloaded(func(e MapEntry) error { out = append(out, e); return nil })
	return out, err
}

// LookupMany resolves a batch of map_uids in a single lock hop and one forward
// pass over the base. Returns one MapEntry per requested UID; entries that did
// not resolve carry UID=0. The result slice mirrors the input order.
func (m *Map) LookupMany(mapUIDs []uint32) ([]MapEntry, error) {
	lockRead(&m.mu)
	defer m.mu.Unlock()
	if !m.loaded {
		return make([]MapEntry, len(mapUIDs)), nil
	}
	var found map[uint32]MapEntry
	if err := m.retryBaseLocked(func() (err error) {
		found, err = m.st.lookupMany(mapUIDs)
		return err
	}); err != nil {
		return nil, fmt.Errorf("mdboxmap/lookup: %w", err)
	}
	var missed []uint32
	for _, uid := range mapUIDs {
		if _, ok := found[uid]; !ok {
			missed = append(missed, uid)
		}
	}
	if len(missed) > 0 {
		// Refresh once to pick up a sibling's appends.
		if rerr := m.reloadLocked(); rerr != nil {
			return nil, fmt.Errorf("mdboxmap/lookup refresh: %w", rerr)
		}
		var again map[uint32]MapEntry
		if err := m.retryBaseLocked(func() (err error) {
			again, err = m.st.lookupMany(missed)
			return err
		}); err != nil {
			return nil, fmt.Errorf("mdboxmap/lookup: %w", err)
		}
		for uid, e := range again {
			found[uid] = e
		}
	}
	out := make([]MapEntry, len(mapUIDs))
	for i, uid := range mapUIDs {
		out[i] = found[uid]
	}
	return out, nil
}

// LookupByGUID finds the map_uid for a message GUID -- the rebuild's preferred
// match, since a GUID survives compaction where an offset does not. A zero GUID
// always returns false, or it would mass-match every pre-GUID record.
func (m *Map) LookupByGUID(guid [16]byte) (MapEntry, bool, error) {
	guidWalks.Add(1)
	if guid == ([16]byte{}) {
		return MapEntry{}, false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var hit MapEntry
	err := m.st.scan(func(e MapEntry) error {
		if e.GUID == guid {
			hit = e
			return errStopWalk
		}
		return nil
	})
	if errors.Is(err, errStopWalk) {
		return hit, true, nil
	}
	if err != nil {
		return MapEntry{}, false, fmt.Errorf("mdboxmap/lookup guid: %w", err)
	}
	return MapEntry{}, false, nil
}

// errStopWalk ends a walk that has its answer.
var errStopWalk = errors.New("mdboxmap: walk stopped")
