package file

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
)

// baseImage is one version of a base file, parsed once and never written to
// again: compaction makes a new image, old readers keep the old one (#1809).
type baseImage struct {
	file     *mailindex.File // no Records: they are recs, loaded by chunk
	recs     *mailindex.Records
	keywords keywordsHdr
	lineage  lineageHdr
	ident    os.FileInfo
	size     int64
	mod      time.Time
}

// baseImages keeps one parse per base version, shared between readers. Keyed
// by path; the identity check says whether it still describes the file.
var baseImages sync.Map // indexPath -> *baseImage

// baseReadAttempts bounds the re-reads a base that keeps moving costs.
const baseReadAttempts = 3

// afterBaseRead is a test seam: another process replacing the base between the
// read and the stat that labels it. Nil everywhere else.
var afterBaseRead func()

// replaySeam and buildViewSeam are test seams: a replay that stops before a
// record, a rebuild that fails. Nil everywhere else.
var (
	replaySeam    func() error
	buildViewSeam func() error
)

// decodedImages is a test seam: images decoded whole, as before the lazy
// form, so a row can compare the two. False everywhere else.
var decodedImages bool

// afterViewImage is a test seam: the base and log replaced between the image a
// view is built on and the log it opens. Nil everywhere else.
var afterViewImage func()

// imageFor returns the parsed base for the file at fs.indexPath as it is now.
// It takes no lock and writes nothing the writer can see.
func imageFor(fs *folderState) (*baseImage, error) {
	st, err := os.Stat(fs.indexPath)
	if err != nil {
		return nil, err
	}
	if cached, ok := baseImages.Load(fs.indexPath); ok && !decodedImages {
		img, _ := cached.(*baseImage)
		if img != nil && img.ident != nil && os.SameFile(img.ident, st) && img.size == st.Size() && img.mod.Equal(st.ModTime()) {
			return img, nil
		}
	}
	// The label has to describe what was read: a base replaced mid-read would
	// otherwise cache old content under the new file's identity (#2056).
	var img *baseImage
	for attempt := 0; ; attempt++ {
		pre := st
		parsed, recs, perr := mailindex.OpenLazy(fs.indexPath)
		if decodedImages {
			parsed, perr = mailindex.Open(fs.indexPath)
			recs = nil
		}
		if perr != nil {
			return nil, asCorrupt(fs.folder, perr)
		}
		if afterBaseRead != nil {
			afterBaseRead()
		}
		after, serr := os.Stat(fs.indexPath)
		if serr != nil {
			return nil, serr
		}
		if os.SameFile(pre, after) && pre.Size() == after.Size() && pre.ModTime().Equal(after.ModTime()) {
			img = &baseImage{file: parsed, recs: recs, ident: pre, size: pre.Size(), mod: pre.ModTime()}
			break
		}
		st = after
		if attempt >= baseReadAttempts {
			// Labelled with the file it came from: older, never wrong, and the
			// next read sees the file has moved.
			img = &baseImage{file: parsed, recs: recs, ident: pre, size: pre.Size(), mod: pre.ModTime()}
			metricBaseReadGaveUp.Inc()
			break
		}
	}
	parsed := img.file
	if ext := findExt(parsed.Extensions, extNameKeywords); ext != nil {
		kw, kerr := decodeKeywordsHdr(ext.HdrData)
		if kerr != nil {
			return nil, fmt.Errorf("fileindex/view: keywords: %w", kerr)
		}
		img.keywords = kw
	}
	if ext := findExt(parsed.Extensions, extNameLineage); ext != nil {
		img.lineage = decodeLineageHdr(ext.HdrData)
	}
	if !decodedImages {
		baseImages.Store(fs.indexPath, img)
	}
	return img, nil
}

// buildView folds one image: the shared base parse plus the log tail replayed
// into private memory, up to the last whole group (#1833). It reports the
// offset the view stands at -- a base that already holds the whole log stands
// at the log's end, not at zero, and the next fold continues from there.
func (fs *folderState) buildView() (*folderState, int64, *baseImage, error) {
	if buildViewSeam != nil {
		if err := buildViewSeam(); err != nil {
			return nil, 0, nil, err
		}
	}
	img, lg, err := fs.imageAndItsLog()
	if err != nil {
		return nil, 0, nil, err
	}
	defer lg.close()
	// A handle stamps traceID under fs.mu while this reader holds nothing.
	fs.mu.RLock()
	traceID := fs.traceID
	fs.mu.RUnlock()
	view := &folderState{
		user:      fs.user,
		folder:    fs.folder,
		indexDir:  fs.indexDir,
		indexPath: fs.indexPath,
		traceID:   traceID,
		file:      img.file,
		recs:      img.recs,
		keywords:  img.keywords,
		lineage:   img.lineage,
	}

	if lg.f == nil || !lg.ok {
		if rerr := view.refreshExtState(); rerr != nil {
			return nil, 0, nil, rerr
		}
		if verr := view.ensureVsizeView(); verr != nil {
			return nil, 0, nil, verr
		}
		view.onBase = view.recs != nil
		return view, 0, img, nil
	}
	// Where this log meets this base, by the lineage table: an unpaired one
	// replays whole rather than from an offset that means nothing in it.
	from, paired := replayStart(img.lineage, lg.lineage())
	if !paired {
		from = int64(mailindex.LogHeaderSize)
	}
	if lg.size <= from {
		if rerr := view.refreshExtState(); rerr != nil {
			return nil, 0, nil, rerr
		}
		if verr := view.ensureVsizeView(); verr != nil {
			return nil, 0, nil, verr
		}
		view.onBase = view.recs != nil
		return view, lg.size, img, nil // the base already holds everything
	}

	// The tail goes onto a fork of the shared image's records, which copies a
	// chunk before changing it: the image stays as every other view reads it.
	if err := view.forkRecords(); err != nil {
		return nil, 0, nil, err
	}
	end, aerr := view.applyLogFrom(lg, from)
	if aerr != nil {
		return nil, 0, nil, aerr
	}
	view.logSize = end
	// The typed halves of the header the readers ask for -- keywords, vsize,
	// dbox -- are derived, and a view that skips them answers zero.
	if rerr := view.refreshExtState(); rerr != nil {
		return nil, 0, nil, rerr
	}
	// A recount that finds no sizes at all does not replace an aggregate the
	// base already knows: records carrying none add nothing (#1728).
	fromHeader := view.vsize
	view.ensureVsizeLocked()
	if view.vsize.Vsize == 0 && fromHeader.Vsize > 0 {
		view.vsize = fromHeader
	}
	return view, end, img, nil
}

// errBaseMoving says the base kept being replaced while a view was built; the
// caller reads under the lock instead.
var errBaseMoving = errors.New("fileindex/view: the base kept moving while the view was built")

// imageAndItsLog returns a base image and a log opened while that base was
// still the file: a base replaced in between pairs it with the next base's log.
func (fs *folderState) imageAndItsLog() (*baseImage, *logReader, error) {
	for attempt := 0; ; attempt++ {
		img, err := imageFor(fs)
		if err != nil {
			return nil, nil, err
		}
		if afterViewImage != nil {
			afterViewImage()
		}
		lg, lgErr := openLogRead(fs.indexPath)
		if lgErr != nil {
			return nil, nil, fmt.Errorf("fileindex/view: log: %w", lgErr)
		}
		st, serr := os.Stat(fs.indexPath)
		if serr == nil && os.SameFile(img.ident, st) && img.size == st.Size() && img.mod.Equal(st.ModTime()) {
			return img, lg, nil
		}
		lg.close()
		if serr != nil {
			return nil, nil, serr
		}
		if attempt >= baseReadAttempts {
			return nil, nil, errBaseMoving
		}
	}
}

// cloneIndexFile copies what a replay writes into: the records and their
// extension data. The header and extension list are copied by value.
func cloneIndexFile(src *mailindex.File) *mailindex.File {
	out := *src
	out.Records = make([]*mailindex.Record, len(src.Records))
	for i, rec := range src.Records {
		cp := *rec
		if rec.Ext != nil {
			cp.Ext = make(map[string][]byte, len(rec.Ext))
			for k, v := range rec.Ext {
				b := make([]byte, len(v))
				copy(b, v)
				cp.Ext[k] = b
			}
		}
		out.Records[i] = &cp
	}
	out.Extensions = make([]mailindex.Extension, len(src.Extensions))
	copy(out.Extensions, src.Extensions)
	for i := range out.Extensions {
		if h := src.Extensions[i].HdrData; h != nil {
			b := make([]byte, len(h))
			copy(b, h)
			out.Extensions[i].HdrData = b
		}
	}
	return &out
}
