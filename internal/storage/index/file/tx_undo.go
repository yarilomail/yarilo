package file

import (
	"log/slog"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// txUndo is the folder state as it was before a transaction applied anything:
// the ops mutate fs.file first and the log is written last, so a log that
// refuses leaves memory ahead of the disk unless this puts it back (#1831).
type txUndo struct {
	header  mailindex.Header
	extHdr  map[string][]byte
	keyword keywordsHdr
	vsize   hdrVsize
	// records is taken only for a transaction that adds or removes one: those
	// shift the slice in place, and a per-record pre-image cannot undo that.
	records []*mailindex.Record
	recs    map[uint32]*mailindex.Record
	// metas is the caller's own message: an append stamps uid and modseq on it,
	// and a uid nothing was written under must not travel back to the caller.
	metas   []metaUndo
	flushes uint64
}

type metaUndo struct {
	m      *mailbox.MessageMeta
	uid    uint32
	modseq uint64
}

// snapshotForTx takes the pre-image of everything t's ops can reach.
func (fs *folderState) snapshotForTx(ops []txOp) *txUndo {
	u := &txUndo{
		header:  fs.file.Header,
		extHdr:  make(map[string][]byte, len(fs.file.Extensions)),
		keyword: keywordsHdr{Names: append([]string(nil), fs.keywords.Names...)},
		vsize:   fs.vsize,
		recs:    make(map[uint32]*mailindex.Record),
		flushes: fs.flushes,
	}
	for _, ext := range fs.file.Extensions {
		if ext.HdrData != nil {
			u.extHdr[ext.Name] = append([]byte(nil), ext.HdrData...)
		}
	}
	structural := false
	touched := make(map[uint32]struct{}, len(ops))
	for i := range ops {
		switch ops[i].kind {
		case opAppend, opExpunge:
			structural = true
		}
		if ops[i].kind == opAppend && ops[i].meta != nil {
			u.metas = append(u.metas, metaUndo{m: ops[i].meta, uid: ops[i].meta.UID, modseq: ops[i].meta.ModSeq})
		}
		if ops[i].uid != 0 {
			touched[ops[i].uid] = struct{}{}
		}
	}
	if structural {
		u.records = append([]*mailindex.Record(nil), fs.file.Records...)
	}
	for _, rec := range fs.file.Records {
		if _, ok := touched[rec.UID]; !ok {
			continue
		}
		u.recs[rec.UID] = cloneRecord(rec)
	}
	return u
}

// restore puts the folder back to the pre-image. The records are restored
// through the pointers the slice already holds: a view sharing one sees the
// value it had, not a record that was never written down.
func (fs *folderState) restore(u *txUndo) {
	fs.file.Header = u.header
	for i := range fs.file.Extensions {
		if hdr, ok := u.extHdr[fs.file.Extensions[i].Name]; ok {
			fs.file.Extensions[i].HdrData = append([]byte(nil), hdr...)
		}
	}
	fs.keywords = u.keyword
	fs.vsize = u.vsize
	for _, m := range u.metas {
		m.m.UID = m.uid
		m.m.ModSeq = m.modseq
	}
	if u.records != nil {
		fs.file.Records = u.records
	}
	for _, rec := range fs.file.Records {
		if pre, ok := u.recs[rec.UID]; ok {
			*rec = *cloneRecord(pre)
		}
	}
	// A transaction whose first append registered a keyword name wrote a base
	// mid-flight; restored memory has to reach the disk or the two disagree.
	if fs.flushes != u.flushes {
		if err := fs.flush(); err != nil {
			slog.Error("fileindex/tx: the base still holds a transaction that was not logged",
				"folder", fs.folder, "err", err)
		}
	}
}

func cloneRecord(rec *mailindex.Record) *mailindex.Record {
	cp := *rec
	if rec.Ext != nil {
		cp.Ext = make(map[string][]byte, len(rec.Ext))
		for k, v := range rec.Ext {
			cp.Ext[k] = append([]byte(nil), v...)
		}
	}
	return &cp
}
