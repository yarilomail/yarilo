package file

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
)

// baseImage is one version of a folder's base file, parsed once and never
// written to again. Compaction replaces the file, which makes a new image;
// a reader holding the old one keeps reading it (#1809).
type baseImage struct {
	file     *mailindex.File
	keywords keywordsHdr
	lineage  lineageHdr
	// logEnd is the log offset this base already absorbed, where a reader's
	// own replay starts.
	logEnd int64
	ident  os.FileInfo
	size   int64
	mod    time.Time
}

// baseImages keeps the parsed image for a base file, so concurrent readers of
// one version parse it once between them. Keyed by path; the identity check
// decides whether the entry still describes the file on disk.
var baseImages sync.Map // indexPath -> *baseImage

// imageFor returns the parsed base for the file at fs.indexPath as it is now.
// It takes no lock and writes nothing the writer can see.
func imageFor(fs *folderState) (*baseImage, error) {
	st, err := os.Stat(fs.indexPath)
	if err != nil {
		return nil, err
	}
	if cached, ok := baseImages.Load(fs.indexPath); ok {
		img := cached.(*baseImage)
		if img.ident != nil && os.SameFile(img.ident, st) && img.size == st.Size() && img.mod.Equal(st.ModTime()) {
			return img, nil
		}
	}
	parsed, err := mailindex.Open(fs.indexPath)
	if err != nil {
		return nil, asCorrupt(fs.folder, err)
	}
	// Identity taken after the read, and only trusted when it still matches
	// the stat that chose this parse: a compaction in between makes a new
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

// readSnapshot builds this reader's own view: the shared base image plus the
// log tail up to the last transaction boundary, replayed into private memory.
// A torn record after that boundary is not a state anything may read (#1831).
func (fs *folderState) readSnapshot() (*folderState, error) {
	img, err := imageFor(fs)
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("fileindex/view: log: %w", lgErr)
	}
	defer lg.close()
	if lg.f == nil || !lg.ok {
		if rerr := view.refreshExtState(); rerr != nil {
			return nil, rerr
		}
		view.ensureVsizeLocked()
		return view, nil
	}
	// Where this log meets this base, by the lineage table: an unpaired one
	// replays whole rather than from an offset that means nothing in it.
	from, paired := replayStart(img.lineage, lg.lineage())
	if !paired {
		from = int64(mailindex.LogHeaderSize)
	}
	if lg.size <= from {
		if rerr := view.refreshExtState(); rerr != nil {
			return nil, rerr
		}
		view.ensureVsizeLocked()
		return view, nil // the base already holds everything
	}

	// Only now is a copy paid for: the tail has something to apply, and the
	// shared image must not carry it.
	view.file = cloneIndexFile(img.file)
	end, aerr := view.applyLogFrom(lg, from)
	if aerr != nil {
		return nil, aerr
	}
	view.logSize = end
	// The typed halves of the header the readers ask for -- keywords, vsize,
	// dbox -- are derived, and a view that skips them answers zero.
	if rerr := view.refreshExtState(); rerr != nil {
		return nil, rerr
	}
	// The aggregate the base wrote is kept when the recount finds no sizes at
	// all: records carrying none are counted and add nothing, so a recount
	// would answer zero for a folder whose size is known (#1728).
	fromHeader := view.vsize
	view.ensureVsizeLocked()
	if view.vsize.Vsize == 0 && fromHeader.Vsize > 0 {
		view.vsize = fromHeader
	}
	return view, nil
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
