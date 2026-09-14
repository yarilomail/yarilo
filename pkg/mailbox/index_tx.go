package mailbox

// IndexTx accumulates one command's changes and writes them once: one hold, one
// record set in the log, one modseq bump, all or nothing (#1827).
type IndexTx interface {
	// Expunge removes the record for uid. A uid the folder does not hold is
	// not an error: another session may have expunged it first.
	Expunge(uid uint32)

	// Append records a message. A zero m.UID is allocated at commit, in the
	// same hold, and written back into m.
	Append(m *MessageMeta)

	// Commit writes everything queued, or nothing. The modseq it reports is
	// the folder's after the write.
	Commit() (modseq uint64, err error)

	// Rollback discards the transaction. Safe after Commit, where it does
	// nothing, so a deferred Rollback is the ordinary shape.
	Rollback()
}

// TxIndex is a UserIndex that can open a transaction. The per-message methods
// remain a transaction of one operation, so there is no second contract.
type TxIndex interface {
	Begin(folderID uint64) (IndexTx, error)
}

// BeginTx opens a transaction on idx, for a caller holding the interface rather
// than the implementation.
func BeginTx(idx UserIndex, folderID uint64) (IndexTx, bool) {
	tx, ok := idx.(TxIndex)
	if !ok {
		return nil, false
	}
	t, err := tx.Begin(folderID)
	if err != nil {
		return nil, false
	}
	return t, true
}
