package file

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
)

// baseImage is one version of a base file, parsed once and never written to
// again: compaction makes a new image, old readers keep the old one (#1809).
type baseImage struct {
	file     *mailindex.File
	keywords keywordsHdr
	lineage  lineageHdr
	ident    os.FileInfo
	size     int64
	mod      time.Time
}

// baseImages keeps one parse per base version, shared between readers. Keyed
// by path; the identity check says whether it still describes the file.
var baseImages sync.Map // indexPath -> *baseImage

// imageFor returns the parsed base for the file at fs.indexPath as it is now.
// It takes no lock and writes nothing the writer can see.
func imageFor(fs *folderState) (*baseImage, error) {
	st, err := os.Stat(fs.indexPath)
	if err != nil {
		return nil, err
	}
	if cached, ok := baseImages.Load(fs.indexPath); ok {
		img, _ := cached.(*baseImage)
		if img != nil && img.ident != nil && os.SameFile(img.ident, st) && img.size == st.Size() && img.mod.Equal(st.ModTime()) {
			return img, nil
		}
	}
	parsed, err := mailindex.Open(fs.indexPath)
	if err != nil {
		return nil, asCorrupt(fs.folder, err)
	}
	// Identity taken after the read: a compaction in between makes a new
	// image rather than a mislabelled one.
	after, serr := os.Stat(fs.indexPath)
	if serr != nil {
		return nil, serr
	}
	img := &baseImage{file: parsed, ident: after, size: after.Size(), mod: after.ModTime()}
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
	baseImages.Store(fs.indexPath, img)
	return img, nil
}

// buildView folds one image: the shared base parse plus the log tail replayed
// into private memory, up to the last whole group (#1833). It reports the
// offset the view stands at -- a base that already holds the whole log stands
// at the log's end, not at zero, and the next fold continues from there.
func (fs *folderState) buildView() (*folderState, int64, error) {
	img, err := imageFor(fs)
	if err != nil {
		return nil, 0, err
	}
	view := &folderState{
		user:      fs.user,
		folder:    fs.folder,
		indexDir:  fs.indexDir,
		indexPath: fs.indexPath,
		traceID:   fs.traceID,
		file:      img.file,
		keywords:  img.keywords,
		lineage:   img.lineage,
	}

	lg, lgErr := openLogRead(fs.indexPath)
	if lgErr != nil {
		return nil, 0, fmt.Errorf("fileindex/view: log: %w", lgErr)
	}
	defer lg.close()
	if lg.f == nil || !lg.ok {
		if rerr := view.refreshExtState(); rerr != nil {
			return nil, 0, rerr
		}
		view.ensureVsizeLocked()
		return view, 0, nil
	}
	// Where this log meets this base, by the lineage table: an unpaired one
	// replays whole rather than from an offset that means nothing in it.
	from, paired := replayStart(img.lineage, lg.lineage())
	if !paired {
		from = int64(mailindex.LogHeaderSize)
	}
	if lg.size <= from {
		if rerr := view.refreshExtState(); rerr != nil {
			return nil, 0, rerr
		}
		view.ensureVsizeLocked()
		return view, lg.size, nil // the base already holds everything
	}

	// Only now is a copy paid for: the tail has something to apply, and the
	// shared image must not carry it.
	view.file = cloneIndexFile(img.file)
	end, aerr := view.applyLogFrom(lg, from)
	if aerr != nil {
		return nil, 0, aerr
	}
	view.logSize = end
	// The typed halves of the header the readers ask for -- keywords, vsize,
	// dbox -- are derived, and a view that skips them answers zero.
	if rerr := view.refreshExtState(); rerr != nil {
		return nil, 0, rerr
	}
	// A recount that finds no sizes at all does not replace an aggregate the
	// base already knows: records carrying none add nothing (#1728).
	fromHeader := view.vsize
	view.ensureVsizeLocked()
	if view.vsize.Vsize == 0 && fromHeader.Vsize > 0 {
		view.vsize = fromHeader
	}
	return view, end, nil
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
