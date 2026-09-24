package file

import (
	"fmt"
	"log/slog"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Begin opens a transaction on one folder. Nothing is written until Commit, and
// then under one hold, as one record set in the log (#1827).
func (u *userIndex) Begin(folderID uint64) (mailbox.IndexTx, error) {
	u.mu.Lock()
	_, ok := u.open[folderID]
	u.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("fileindex: folder %d not open", folderID)
	}
	return &indexTx{idx: u, folderID: folderID}, nil
}

// txOp is one queued change. The kind decides which fields carry meaning.
type txOp struct {
	kind  txKind
	uid   uint32
	meta  *mailbox.MessageMeta
	upd   mailbox.FlagsUpdate
	dirty bool
}

type txKind uint8

const (
	opExpunge txKind = iota
	opAppend
	opUpdateFlags
	opMarkDirty
)

// indexTx accumulates one command's changes. Queueing touches nothing on disk.
type indexTx struct {
	idx      *userIndex
	folderID uint64
	ops      []txOp
	done     bool
}

func (t *indexTx) Expunge(uid uint32) {
	t.ops = append(t.ops, txOp{kind: opExpunge, uid: uid})
}

func (t *indexTx) Append(m *mailbox.MessageMeta) {
	t.ops = append(t.ops, txOp{kind: opAppend, meta: m})
}

func (t *indexTx) UpdateFlags(uid uint32, upd mailbox.FlagsUpdate) {
	t.ops = append(t.ops, txOp{kind: opUpdateFlags, uid: uid, upd: upd})
}

func (t *indexTx) MarkDirty(uid uint32, dirty bool) {
	t.ops = append(t.ops, txOp{kind: opMarkDirty, uid: uid, dirty: dirty})
}

func (t *indexTx) Rollback() { t.done = true }

// Commit writes every queued change, or none: the records reach the log in one
// append, so a torn transaction cannot be read back, and a write the log refuses
// leaves the folder as it was (#1831).
func (t *indexTx) Commit() (mailbox.TxResult, error) {
	var out mailbox.TxResult
	if t.done {
		return out, fmt.Errorf("fileindex/tx: already finished")
	}
	t.done = true
	if len(t.ops) == 0 {
		return out, nil
	}
	err := t.idx.withFolderSite(t.folderID, lockSiteTransaction, func(fs *folderState) error {
		// The ops apply to fs.file before the log takes them, so the pre-image
		// is what a refused write is put back to (#1831).
		undo := fs.snapshotForTx(t.ops)
		err := t.applyAll(fs, &out)
		if err != nil {
			fs.restore(undo)
			out = mailbox.TxResult{}
		}
		return err
	})
	if err != nil {
		return mailbox.TxResult{}, err
	}
	return out, nil
}

// applyAll applies every queued op and writes them as one group. Anything it
// returns an error from leaves fs half-applied; Commit puts it back.
func (t *indexTx) applyAll(fs *folderState, out *mailbox.TxResult) error {
	var err error
	out.ModSeq, err = fs.bumpModSeqHeader()
	if err != nil {
		return err
	}
	var records [][]byte
	// Flag changes go as one batch: each message keeps its own modseq,
	// which is what CONDSTORE addresses them by.
	flags := make(map[uint32]mailbox.FlagsUpdate)
	for i := range t.ops {
		if t.ops[i].kind == opUpdateFlags {
			flags[t.ops[i].uid] = t.ops[i].upd
		}
	}
	if len(flags) > 0 {
		out.Flags = make(map[uint32]mailbox.FlagsResult, len(flags))
		recs, ferr := fs.flagsMultiLocked(flags, out.Flags)
		if ferr != nil {
			return ferr
		}
		records = append(records, recs...)
	}
	for i := range t.ops {
		op := &t.ops[i]
		if op.kind == opUpdateFlags {
			continue
		}
		recs, oerr := t.applyLocked(fs, op, out.ModSeq)
		if oerr != nil {
			return oerr
		}
		records = append(records, recs...)
	}
	if len(records) == 0 {
		return nil
	}
	if err := fs.appendMutLog(records...); err != nil {
		return err
	}
	// After the folder's write, never before it: a refused folder write must
	// leave the store without the copy (#1711, conversations.c:2600-2640).
	t.trackGUIDs(fs)
	return nil
}

// trackGUIDs records this command's copies in the per-user store, in one hold:
// a transaction takes the folder once, and the store once (#1827).
func (t *indexTx) trackGUIDs(fs *folderState) {
	var b guidBatch
	for i := range t.ops {
		op := &t.ops[i]
		switch op.kind {
		case opAppend:
			if op.meta.GUID == ([16]byte{}) {
				continue
			}
			b.add = append(b.add, mailbox.GUIDRecord{
				GUID:         op.meta.GUID,
				FolderID:     t.folderID,
				UID:          op.meta.UID,
				InternalDate: op.meta.InternalDate.Unix(),
			})
		case opExpunge:
			b.gone = append(b.gone, guidCopy{folderID: t.folderID, uid: op.uid})
		}
	}
	// The store is derived: a failure is logged and rebuilt, never returned,
	// because the mail is written and the client was told so.
	if err := t.idx.applyGUIDBatch(b); err != nil {
		metricGUIDTrackFailed.Inc()
		slog.Warn("fileindex: the guid store did not take this command; it is derived and rebuildable",
			"user", t.idx.username, "folder", fs.folder, "err", err)
	}
}

func (t *indexTx) applyLocked(fs *folderState, op *txOp, modseq uint64) ([][]byte, error) {
	switch op.kind {
	case opExpunge:
		return fs.expungeLocked(op.uid, modseq)
	case opAppend:
		next := fs.file.Header.NextUID
		if next == 0 {
			next = 1
		}
		if op.meta.UID == 0 {
			fs.file.Header.NextUID = next + 1
			op.meta.UID = next
		}
		if err := fs.appendLocked(op.meta); err != nil {
			return nil, err
		}
		return fs.appendLogRecords(fs.file.Records[len(fs.file.Records)-1])
	case opMarkDirty:
		return fs.markDirtyLocked(op.uid, op.dirty)
	}
	return nil, fmt.Errorf("fileindex/tx: unknown operation %d", op.kind)
}

// Begin on the session handle stamps the folder the way every other write does,
// then opens the transaction on the shared index beneath it.
func (h *userHandle) Begin(folderID uint64) (mailbox.IndexTx, error) {
	return h.stamped(folderID).Begin(folderID)
}
