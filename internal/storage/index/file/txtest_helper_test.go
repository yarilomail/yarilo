package file

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// updateFlagsMulti is the batch a test used to call directly, now one
// transaction: the same write, through the contract that replaced it (#1827).
type flagBatchWriter interface {
	Begin(folderID uint64) (mailbox.IndexTx, error)
}

func updateFlagsMulti(t *testing.T, idx flagBatchWriter, folderID uint64, updates map[uint32]mailbox.FlagsUpdate) (map[uint32]mailbox.FlagsResult, error) {
	t.Helper()
	tx, err := idx.Begin(folderID)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for uid, upd := range updates {
		tx.UpdateFlags(uid, upd)
	}
	out, cerr := tx.Commit()
	if cerr != nil {
		return nil, cerr
	}
	return out.Flags, nil
}
