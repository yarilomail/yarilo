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
}

type txKind uint8

const (
	opExpunge txKind = iota
	opAppend
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

func (t *indexTx) Rollback() { t.done = true }

// Commit writes every queued change, or none: the records reach the log in one
// append, so a torn transaction cannot be read back.
func (t *indexTx) Commit() (uint64, error) {
	if t.done {
		return 0, fmt.Errorf("fileindex/tx: already finished")
	}
	t.done = true
	if len(t.ops) == 0 {
		return 0, nil
	}
	var modseq uint64
	err := t.idx.withFolder(t.folderID, func(fs *folderState) error {
		var err error
		// One bump for the command, not one per message: the modseq a client
		// is told is the folder's after the whole change.
		modseq, err = fs.bumpModSeqHeader()
		if err != nil {
			return err
		}
		var records [][]byte
		for i := range t.ops {
			op := &t.ops[i]
			recs, oerr := t.applyLocked(fs, op, modseq)
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
		return 0, err
	}
	return modseq, nil
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

var _ mailbox.TxIndex = (*userIndex)(nil)

// Begin on the session handle stamps the folder the way every other write does,
// then opens the transaction on the shared index beneath it.
func (h *userHandle) Begin(folderID uint64) (mailbox.IndexTx, error) {
	return h.stamped(folderID).Begin(folderID)
}

var _ mailbox.TxIndex = (*userHandle)(nil)
