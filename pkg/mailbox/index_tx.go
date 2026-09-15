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

	// UpdateFlags queues one message's flag change. The resulting set is
	// resolved at commit against the record the hold finds, and reported back.
	UpdateFlags(uid uint32, upd FlagsUpdate)

	// Commit writes everything queued, or nothing.
	Commit() (TxResult, error)

	// Rollback discards the transaction. Safe after Commit, where it does
	// nothing, so a deferred Rollback is the ordinary shape.
	Rollback()
}

// TxResult is the folder's modseq after the write and the settled flags of
// every message the transaction changed, which a delta is known by only then.
type TxResult struct {
	ModSeq uint64
	Flags  map[uint32]FlagsResult
}
