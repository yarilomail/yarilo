package file

import (
	"errors"
	"os"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
)

// errFoldNotPossible says the tail cannot be applied onto the map we hold; the
// caller rebuilds from the base, which always can.
var errFoldNotPossible = errors.New("fileindex/map: the log cannot extend this image")

// indexMap is one version of a folder's index as unlocked readers see it: the
// base image with the log folded in up to committedEnd. Published maps are
// read-only, so every view of one shares it instead of copying it (#1875).
type indexMap struct {
	view *folderState // read-only once published

	// img is the parsed base this map was folded from. Identity by pointer:
	// imageFor already owns the question of whether the file moved, and a
	// second answer to it out of stat fields disagrees with the first.
	img *baseImage

	// log identity, not only its length: a flush replaces the log, and the
	// tail of the new one is not a continuation of the old one's offset.
	logIdent os.FileInfo
	// logSeen is how long the log was when this map was folded; committedEnd
	// is how far the fold got. They differ when the tail ends mid-group, and
	// the map is still current for that log (#1833).
	logSeen      int64
	committedEnd int64 // log offset folded into view
	refs         int   // live views; guarded by folderState.mapMu

	// w writes the map's chunks for the next version; view.recs is its
	// snapshot, released once the map is neither current nor read. Nil for
	// decoded records and for a view on the shared image.
	w        *mailindex.Records
	released bool
	// broken says a fold onto w stopped part way: w holds some of a tail, so
	// this map is never extended again, only rebuilt from the base.
	broken bool

	// ownsFile says the records are this map's own. A view built from a base
	// with nothing in the log shares the cached base image, and extending it
	// in place would rewrite what every other folder state reads.
	ownsFile bool
}

// foldsOf counts the folds, and copies counts the record copies a fold paid.
// Test-facing: "one fold, not two" is the property the race row asserts.
var (
	folds     counter
	mapCopies counter
)

type counter struct {
	mu sync.Mutex
	n  int
}

func (c *counter) add()      { c.mu.Lock(); c.n++; c.mu.Unlock() }
func (c *counter) load() int { c.mu.Lock(); defer c.mu.Unlock(); return c.n }

// Folds and MapCopies report the two seams this change is measured on.
func Folds() int     { return folds.load() }
func MapCopies() int { return mapCopies.load() }

// ResetMapCounters zeroes both, for a row that counts one folder's work.
func ResetMapCounters() {
	folds.mu.Lock()
	folds.n = 0
	folds.mu.Unlock()
	mapCopies.mu.Lock()
	mapCopies.n = 0
	mapCopies.mu.Unlock()
}

// openView hands out the current image, folding the log tail into a new one
// first when the log has grown. Nothing is held while the tail is read: the
// mutex covers the pointer and the count, never a file.
func (fs *folderState) openView() (*folderState, func(), error) {
	img, ierr := imageFor(fs)
	logSt := logStatOf(fs.indexPath)

	if v, rel, ok := fs.viewIfCurrent(img, logSt, ierr); ok {
		return v, rel, nil
	}

	// One fold, not one per reader: the others wait here and then find the
	// map this one published.
	fs.foldMu.Lock()
	defer fs.foldMu.Unlock()

	img, ierr = imageFor(fs)
	logSt = logStatOf(fs.indexPath)
	if v, rel, ok := fs.viewIfCurrent(img, logSt, ierr); ok {
		return v, rel, nil
	}

	m, err := fs.fold(img, logSt, ierr)
	if err != nil {
		return nil, nil, err
	}
	fs.mapMu.Lock()
	old := fs.current
	fs.current = m
	m.refs++
	if old != nil && old != m {
		fs.retireLocked(old)
	}
	fs.mapMu.Unlock()
	return m.view, func() { fs.releaseView(m) }, nil
}

// viewIfCurrent takes a reference to the published map when it already holds
// this base and this much of the log.
func (fs *folderState) viewIfCurrent(img *baseImage, logSt os.FileInfo, ierr error) (*folderState, func(), bool) {
	if ierr != nil || img == nil {
		return nil, nil, false
	}
	asked := time.Now()
	fs.mapMu.Lock()
	if mapWaitSeam != nil {
		mapWaitSeam(time.Since(asked))
	}
	defer fs.mapMu.Unlock()
	m := fs.current
	if m == nil || !sameImage(m.img, img) || !sameLog(m.logIdent, logSt) || m.logSeen != sizeOfLog(logSt) {
		// A miss is the log having grown, the log having been replaced, or the
		// base having been rewritten; the fold tells them apart.
		return nil, nil, false
	}
	m.refs++
	return m.view, func() { fs.releaseView(m) }, true
}

// dropView unpublishes the map holding view and forgets the cached image of
// the base, so the next read builds from the current one.
func (fs *folderState) dropView(view *folderState) {
	baseImages.Delete(fs.indexPath)
	fs.mapMu.Lock()
	if m := fs.current; m != nil && m.view == view {
		fs.current = nil
		fs.retireLocked(m)
	}
	fs.mapMu.Unlock()
}

func (fs *folderState) releaseView(m *indexMap) {
	fs.mapMu.Lock()
	m.refs--
	fs.retireLocked(m)
	fs.mapMu.Unlock()
}

// publishRecords turns a forked view's writer into the map's: readers get a
// snapshot of it, and the next fold writes on.
func (m *indexMap) publishRecords() {
	if m.view.recs == nil || m.img != nil && m.view.recs == m.img.recs {
		return // decoded, or the shared image's records, which nobody writes
	}
	m.w = m.view.recs
	m.view.recs = m.w.Snapshot()
}

// mapWaitSeam is a test seam: how long a reader waited for mapMu, the lock a
// snapshot is published and taken under. Nil everywhere else.
var mapWaitSeam func(time.Duration)

// sibling is a folderState for the next version of a view: the same folder,
// header and records until the fold makes them its own.
func (v *folderState) sibling() *folderState {
	return &folderState{
		user:      v.user,
		folder:    v.folder,
		indexDir:  v.indexDir,
		indexPath: v.indexPath,
		traceID:   v.traceID,
		file:      v.file,
		recs:      v.recs,
		keywords:  v.keywords,
		lineage:   v.lineage,
		logSize:   v.logSize,
		vsize:     v.vsize,
	}
}

// retire releases a map's snapshot once nobody reads it and it is no longer
// current: the writer may then change its chunks in place.
func (fs *folderState) retire(m *indexMap) {
	fs.mapMu.Lock()
	defer fs.mapMu.Unlock()
	if fs.current == m && m.refs == 0 {
		fs.current = nil
	}
	fs.retireLocked(m)
}

func (fs *folderState) retireLocked(m *indexMap) {
	if m.w == nil || m.released || m.refs > 0 || fs.current == m {
		return
	}
	m.view.recs.Release()
	m.released = true
}

// fold builds the next map. It extends the published one when it can -- same
// base, log only longer -- and rebuilds from the base otherwise. The extension
// copies the records only when a view still holds them; with no holder the
// records are extended in place, which is the whole point of counting.
func (fs *folderState) fold(img *baseImage, logSt os.FileInfo, ierr error) (*indexMap, error) {
	folds.add()

	var from *indexMap
	if ierr == nil && img != nil {
		fs.mapMu.Lock()
		if m := fs.current; m != nil && !m.broken && sameImage(m.img, img) && sameLog(m.logIdent, logSt) &&
			sizeOfLog(logSt) >= m.committedEnd {
			from = m
			// Unpublished for the duration: a map being extended in place
			// must not be handed to anyone mid-fold.
			if m.refs == 0 {
				fs.current = nil
			}
		}
		fs.mapMu.Unlock()
	}

	if from != nil {
		m, err := fs.extend(from)
		if err == nil {
			return m, nil
		}
		// A tail that will not apply onto this map is not a failure of the
		// read: the base plus the whole log still answers.
	}

	// The image the view was built on, not the one taken above: two parses of
	// one base are one image here and two record sets (#2265).
	view, end, used, err := fs.buildView()
	if err != nil {
		return nil, err
	}
	m := &indexMap{view: view, img: used, logIdent: logSt, logSeen: sizeOfLog(logSt), committedEnd: end,
		ownsFile: used.file != view.file}
	m.publishRecords()
	return m, nil
}

// extend folds [committedEnd, end) onto the map we already have.
func (fs *folderState) extend(from *indexMap) (*indexMap, error) {
	lg, err := openLogRead(fs.indexPath)
	if err != nil {
		return nil, err
	}
	defer lg.close()
	if lg.f == nil || !lg.ok || lg.size < from.committedEnd {
		return nil, errFoldNotPossible
	}

	target := from.view
	// Still published is still reachable: a reader holding a log stat from
	// before the append takes it, so only an unpublished map is extended in place.
	fs.mapMu.Lock()
	shared := from.refs > 0 || !from.ownsFile || fs.current == from
	fs.mapMu.Unlock()
	onWriter := false
	switch {
	case from.w != nil || from.view.recs != nil:
		// Chunked: the next version writes through the map's writer, which
		// copies only a chunk a live snapshot still shares (INDEX-MEMORY.md §4).
		target = from.view.sibling()
		if from.w != nil {
			fs.retire(from)
			target.file, target.recs = cloneIndexFile(from.view.file), from.w
			onWriter = true
		} else if err := target.forkRecords(); err != nil {
			return nil, err
		}
	case shared:
		// Somebody is reading these decoded records: they stay as they are,
		// and the fold goes onto a copy (the reference's move-to-private).
		mapCopies.add()
		target = from.view.sibling()
		if err := target.forkRecords(); err != nil {
			return nil, err
		}
	}

	end, aerr := target.applyLogFrom(lg, from.committedEnd)
	if aerr == nil {
		aerr = target.refreshExtState()
	}
	if aerr != nil {
		// Even once the replay turned to decoded records, what it wrote
		// before stays in the writer.
		if onWriter {
			fs.mapMu.Lock()
			from.broken = true
			fs.mapMu.Unlock()
		}
		return nil, aerr
	}
	target.logSize = end
	fromHeader := target.vsize
	target.ensureVsizeLocked()
	if target.vsize.Vsize == 0 && fromHeader.Vsize > 0 {
		target.vsize = fromHeader
	}
	m := &indexMap{view: target, img: from.img, logIdent: from.logIdent, logSeen: lg.size,
		committedEnd: end, ownsFile: true}
	m.publishRecords()
	return m, nil
}

// logStatOf answers with nil for an absent log: no log is a state a map can be
// built on, and it is told apart from a log that is there.
func logStatOf(indexPath string) os.FileInfo {
	st, err := os.Stat(indexPath + ".log")
	if err != nil {
		return nil
	}
	return st
}

func sizeOfLog(st os.FileInfo) int64 {
	if st == nil {
		return 0
	}
	return st.Size()
}

// sameImage compares two parses of the base. Pointer first, then the file
// version they were taken from: two readers racing on the cache each parse the
// same file, and two objects of one version are one version.
func sameImage(a, b *baseImage) bool {
	if a == nil || b == nil {
		return false
	}
	if a == b {
		return true
	}
	return a.ident != nil && b.ident != nil && os.SameFile(a.ident, b.ident) &&
		a.size == b.size && a.mod.Equal(b.mod)
}

// sameLog asks whether this is still the same log file, not whether it is the
// same length: a longer log is what a fold extends from. A replaced log is a
// different file, and a log truncated in place is shorter than what the map
// already folded, which is the other half of the test.
func sameLog(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return os.SameFile(a, b)
}
