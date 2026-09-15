package file

import (
	"fmt"

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
	kind txKind
	uid  uint32
	meta *mailbox.MessageMeta
	upd  mailbox.FlagsUpdate
}

type txKind uint8

const (
	opExpunge txKind = iota
	opAppend
	opUpdateFlags
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

func (t *indexTx) Rollback() { t.done = true }

// Commit writes every queued change, or none: the records reach the log in one
// append, so a torn transaction cannot be read back.
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
		return fs.appendMutLog(records...)
	})
	if err != nil {
		return mailbox.TxResult{}, err
	}
	return out, nil
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
	}
	return nil, fmt.Errorf("fileindex/tx: unknown operation %d", op.kind)
}

// Begin on the session handle stamps the folder the way every other write does,
// then opens the transaction on the shared index beneath it.
func (h *userHandle) Begin(folderID uint64) (mailbox.IndexTx, error) {
	return h.stamped(folderID).Begin(folderID)
}
