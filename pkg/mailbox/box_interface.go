package mailbox

import "io"

// Box is one account's mail, both halves at once: a consumer asks it for a
// message and never assembles the pair. Implemented with storage (#1715).
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
	// WriteFlags settles flag changes in storage and marks those that did not.
	WriteFlags(f *Folder, folder string, writes []FlagWrite) []FlagWriteResult
	// ExpungeMarked removes messages and their records under one hold.
	ExpungeMarked(f *Folder, folder string, msgs []*MessageMeta) (removed []uint32, failed int)
	// RemoveMessage unlinks a body, leaving the record to the caller.
	RemoveMessage(folder string, m *MessageMeta) error

	// Store and Index are the halves this arc has not moved yet. Every use is
	// a consumer the series still owes a method (#1715).
	Store() UserMailbox
	Index() UserIndex

	// Close releases both halves.
	Close()
}
