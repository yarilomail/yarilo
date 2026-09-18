package file

import (
	"errors"
	"os"
	"sync"
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

// sameBase reports whether this map was built on the base file that is there
// now: a compaction replaces the file, and its records are not ours to extend.
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
	fs.current = m
	m.refs++
	fs.mapMu.Unlock()
	return m.view, func() { fs.releaseView(m) }, nil
}

// viewIfCurrent takes a reference to the published map when it already holds
// this base and this much of the log.
func (fs *folderState) viewIfCurrent(img *baseImage, logSt os.FileInfo, ierr error) (*folderState, func(), bool) {
	if ierr != nil || img == nil {
		return nil, nil, false
	}
	fs.mapMu.Lock()
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

func (fs *folderState) releaseView(m *indexMap) {
	fs.mapMu.Lock()
	m.refs--
	fs.mapMu.Unlock()
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
		if m := fs.current; m != nil && sameImage(m.img, img) && sameLog(m.logIdent, logSt) &&
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

	view, end, err := fs.buildView()
	if err != nil {
		return nil, err
	}
	return &indexMap{view: view, img: img, logIdent: logSt, logSeen: sizeOfLog(logSt), committedEnd: end,
		ownsFile: img == nil || img.file != view.file}, nil
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
	fs.mapMu.Lock()
	shared := from.refs > 0 || !from.ownsFile
	fs.mapMu.Unlock()
	if shared {
		// Somebody is reading this image: it stays as it is, and the fold
		// goes onto a copy (the reference's move-to-private).
		mapCopies.add()
		target = &folderState{
			user:      from.view.user,
			folder:    from.view.folder,
			indexDir:  from.view.indexDir,
			indexPath: from.view.indexPath,
			traceID:   from.view.traceID,
			file:      cloneIndexFile(from.view.file),
			keywords:  from.view.keywords,
			lineage:   from.view.lineage,
			logSize:   from.view.logSize,
			vsize:     from.view.vsize,
		}
	}

	end, aerr := target.applyLogFrom(lg, from.committedEnd)
	if aerr != nil {
		return nil, aerr
	}
	target.logSize = end
	if rerr := target.refreshExtState(); rerr != nil {
		return nil, rerr
	}
	fromHeader := target.vsize
	target.ensureVsizeLocked()
	if target.vsize.Vsize == 0 && fromHeader.Vsize > 0 {
		target.vsize = fromHeader
	}
	return &indexMap{view: target, img: from.img, logIdent: from.logIdent, logSeen: lg.size,
		committedEnd: end, ownsFile: true}, nil
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
