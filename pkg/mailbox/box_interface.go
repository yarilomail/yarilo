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
	// UpdateFlags applies one delta to one message. A delta, not a set: the
	// index resolves it against the record its own lock finds (#1250).
	UpdateFlags(folderID uint64, uid uint32, upd FlagsUpdate) error
	// POP3UIDLs returns the stable UIDLs an earlier session saved.
	POP3UIDLs(folderID uint64) (map[uint32]string, error)
	// SavePOP3UIDLs persists them, so the next session hands out the same ones.
	SavePOP3UIDLs(folderID uint64, uidls map[uint32]string) error
	// HealCorrupt repairs a folder a driver marked and returns the UIDs it
	// expunged. Zero and nil error when the driver does not heal.
	HealCorrupt(f *Folder) ([]ExpungedCopy, error)
	// MarkCorruptOnFetchErr flags this folder for a heal when err wraps
	// ErrCorruptStorage, and reports whether it marked it.
	MarkCorruptOnFetchErr(folder string, err error) bool
	// WriteFlags settles flag changes in storage and marks those that did not.
	WriteFlags(f *Folder, folder string, writes []FlagWrite) []FlagWriteResult
	// ExpungeMarked removes messages and their records under one hold, and
	// returns what went: a caller tells the world outside the hold (#1853).
	ExpungeMarked(f *Folder, folder string, msgs []*MessageMeta) (removed []*MessageMeta, failed int, err error)
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

	// Poll brings a folder its store derives up to date, as an open does;
	// it answers the refreshed folder when the records moved, nil otherwise.
	Poll(f *Folder) (*Folder, error)
	// Begin opens one command's changes to a folder.
	Begin(folderID uint64) (BoxTx, error)
	// Vanished are the uids expunged past sinceModSeq, for QRESYNC.
	Vanished(folderID uint64, sinceModSeq uint64) ([]uint32, error)
	// VanishedGUIDs are the messages expunged past sinceModSeq, by identity;
	// complete is false when some cannot be named.
	VanishedGUIDs(folderID uint64, sinceModSeq uint64) (guids [][16]byte, complete bool, err error)
	// ExpungeFloor is the oldest modseq the folder's expunge history reaches.
	ExpungeFloor(folderID uint64) (uint64, error)
	// FolderStamp is the cheap proof that a folder has not moved since a read.
	FolderStamp(name string) (FolderStamp, error)
	// GUIDCopies is every copy of these messages the account records;
	// ErrNoGUIDStore when it keeps no such record.
	GUIDCopies(guids [][16]byte) ([]GUIDRecord, error)
	// Keywords are the folder's keyword names.
	Keywords(folderID uint64) ([]string, error)
	// CreateFolder writes a new folder's index with the folder, best effort.
	CreateFolder(name string, uidValidity uint32)
	// DeleteFolder and RenameFolder carry a folder's index state along.
	DeleteFolder(name string) error
	RenameFolder(oldName, newName string) error
	// Metadata is the folder's size and message count as its index keeps them.
	Metadata(folderID uint64) (FolderMetadata, error)
	// RecordExists reports whether the folder still holds uid.
	RecordExists(folderID uint64, uid uint32) bool
	// RebuildFolder rebuilds a lost index from the store; it answers the
	// messages the folder holds after.
	RebuildFolder(f *Folder) (int, error)
	// BackfillGUIDs gives records without a message GUID the one their body has.
	BackfillGUIDs(f *Folder, name string) error
	// EnvelopeCache opens the folder's envelope cache. Never nil: with none
	// served every read misses and every store is dropped.
	EnvelopeCache(folderID uint64, opts EnvelopeCacheOptions) EnvelopeCache

	// Close releases both halves.
	Close()
}

// CountingOpener is a box that can open folders without settling them.
type CountingOpener interface {
	// CountingBox opens folders as they are, for a caller that needs the
	// folder's identity and its index and nothing from the store.
	CountingBox() Box
}

// Counting is b's non-settling view where it has one, and b itself otherwise:
// a driver that does not settle has nothing to take off the path (#1875).
func Counting(b Box) Box {
	if c, ok := b.(CountingOpener); ok {
		return c.CountingBox()
	}
	return b
}
