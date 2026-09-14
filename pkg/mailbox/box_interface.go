package mailbox

import "io"

// Box is one account's mail, both halves at once: a consumer asks it for a
// message and never assembles the pair. Implemented with storage (#1715).
// ExpungeNotify runs inside the hold once a message is gone, for what only the
// protocol knows: its untagged response and its own bookkeeping.
type ExpungeNotify func(m *MessageMeta) error

type Box interface {
	// Username is whose mail this is.
	Username() string
	// Folder opens one folder's index.
	Folder(name string, uidValidity uint32) (*Folder, error)
	// Messages reads records with the driver's fill-ins applied.
	Messages(folderID uint64, set SeqSet) ([]*MessageMeta, error)

	// Readable reports whether a record resolves to a body at all.
	Readable(m *MessageMeta) bool
	// MessagePath is the file a record names.
	MessagePath(folder string, m *MessageMeta) (string, error)
	// OpenMessage opens the body a record names.
	OpenMessage(folder string, m *MessageMeta) (io.ReadCloser, error)
	// RFC822Size is the size a client is told.
	RFC822Size(folder string, m *MessageMeta) uint32
	// MessageSize is both numbers a record reports.
	MessageSize(folder string, m *MessageMeta) (size, vsize uint32, err error)
	// FillResponseSizes fills a slice in memory for one response.
	FillResponseSizes(folder string, msgs []*MessageMeta)
	// FillSizeless persists a size into records that carry none.
	FillSizeless(f *Folder) (int, error)

	// RecordDelivered records a delivery: the name settles before the record.
	RecordDelivered(f *Folder, folder, saved string, m *MessageMeta) error
	// RecordSaved records a body already written into a folder.
	RecordSaved(f *Folder, folder, saved string, m *MessageMeta) error
	// NameSaved settles the name of a body saved under a uid already held.
	NameSaved(folder, saved string, m *MessageMeta) error
	// UpdateFlags applies one delta to one message. A delta, not a set: the
	// index resolves it against the record its own lock finds (#1250).
	UpdateFlags(folderID uint64, uid uint32, upd FlagsUpdate) error
	// POP3UIDLs returns the stable UIDLs an earlier session saved.
	POP3UIDLs(folderID uint64) (map[uint32]string, error)
	// SavePOP3UIDLs persists them, so the next session hands out the same ones.
	SavePOP3UIDLs(folderID uint64, uidls map[uint32]string) error
	// HealCorrupt repairs a folder a driver marked and returns the UIDs it
	// expunged. Zero and nil error when the driver does not heal.
	HealCorrupt(f *Folder) ([]uint32, error)
	// MarkCorruptOnFetchErr flags this folder for a heal when err wraps
	// ErrCorruptStorage, and reports whether it marked it.
	MarkCorruptOnFetchErr(folder string, err error) bool
	// WriteFlags settles flag changes in storage and marks those that did not.
	WriteFlags(f *Folder, folder string, writes []FlagWrite) []FlagWriteResult
	// ExpungeMarked removes messages and their records under one hold. A notify
	// error stops the batch and comes back as notifyErr.
	ExpungeMarked(f *Folder, folder string, msgs []*MessageMeta, notify ExpungeNotify) (removed []uint32, failed int, notifyErr error)
	// HoldFolder runs fn under the storage's folder hold, for a caller whose
	// own multi-step write must not be interleaved (#1794).
	HoldFolder(folder, site string, fn func() error) error
	// RemoveHeld unlinks a body inside a hold the caller already has.
	RemoveHeld(folder, name string) error
	// RemoveMessage unlinks a body, leaving the record to the caller.
	RemoveMessage(folder string, m *MessageMeta) error

	// Store and Index are the halves this arc has not moved yet. Every use is
	// a consumer the series still owes a method (#1715).
	Store() UserMailbox

	// Close releases both halves.
	Close()
}
