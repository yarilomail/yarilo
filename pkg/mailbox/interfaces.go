package mailbox

import (
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"time"
)

// ErrCorruptStorage is wrapped by a driver's read path when a message's backing
// storage is missing, truncated or malformed. Callers detect it via errors.Is and
// mark the folder for a rebuild on next open. Transient I/O errors (EIO, timeouts)
// must NOT be wrapped with it.
var ErrCorruptStorage = errors.New("mailbox: corrupt message storage")

// CorruptionMarker is an optional capability of an IndexBackend handle that
// persists a per-folder "needs rebuild" marker so a corrupt message detected on
// one read triggers a heal on the next open. Kept off the core UserIndex interface;
// callers type-assert. The marker is exposed read-side as Folder.Fsckd.
// UIDSpaceAdopter takes a folder's UID space from what a store already records
// rather than minting a fresh one. Kept off the core UserIndex interface, like
// CorruptionMarker: callers type-assert.
//
// For adopting a store another implementation wrote. A client reconnecting over
// the same mailbox keeps its cache only if both the UIDVALIDITY and the UIDs are
// the ones it saw; a fresh UID space makes it refetch everything, which costs
// what a migration over IMAP costs.
// FlagWriter records a message's flag state where the storage keeps it, and
// returns the name the message now has.
//
// One point, not three: a driver that keeps flags outside the index has to be
// told once, with the whole set, after the index has settled it. Kept off the
// core UserMailbox interface -- the dbox drivers do not implement it, because
// there the flags live in the index by design and the reference does the same.
//
// Maildir is the driver that needs it. A maildir keeps flags in the filename so
// the store describes itself: rebuild the index from the directory and the
// state comes back. A flag change that never reaches the name breaks that --
// the rebuild returns the state each message had when it was delivered, in our
// own store, with no other implementation involved (#1601).
type FlagWriter interface {
	// WriteFlags renames the message to carry flags and keywords, and returns
	// its new filename. The name is unchanged when nothing about it changes,
	// and the caller records whatever comes back.
	WriteFlags(folder, filename string, flags, keywords []string) (string, error)
}

// ErrUIDSpaceInUse says a folder already holds messages, so its UID space is one
// a session may have seen and is not open to being replaced.
//
// Not a failure at the call site: a caller adopting a store asks about every
// folder it opens, and most of them are ordinary folders with mail in them.
var ErrUIDSpaceInUse = errors.New("the folder already holds messages")

type UIDSpaceAdopter interface {
	// AdoptUIDSpace sets the folder's UIDVALIDITY and next UID. It must refuse a
	// folder that already holds messages: rewriting the UID space under a
	// mailbox somebody is already reading is the one thing UIDVALIDITY exists to
	// prevent.
	AdoptUIDSpace(folderID uint64, uidValidity, nextUID uint32) error
}

// GUIDRecord is one copy of one message, as the per-user GUID store keeps it:
// the id a JMAP client names, and where that copy lives (INTERNALS.md §35).
type GUIDRecord struct {
	GUID [16]byte
	// FolderGUID, never a number: a folder's id is assigned per process, so a
	// number recorded today names a different folder tomorrow.
	FolderGUID   [16]byte
	UID          uint32
	Flags        uint32
	InternalDate int64
	CID          uint64 // reserved for JMAP Thread; zero until then
}

// GUIDStoreRebuilder writes the per-user GUID store from what the folders say.
// The store is derived, so this is what repairs it (#1711).
type GUIDStoreRebuilder interface {
	ReplaceGUIDStore(copies []GUIDRecord) error
}

// GUIDResolver answers where a message id lives, from the per-user GUID store
// rather than from a walk of the folders (#1711).
type GUIDResolver interface {
	// GUIDCopies returns every copy recorded for these GUIDs. An empty answer
	// means the store says nothing, not that the message does not exist.
	GUIDCopies(guids [][16]byte) ([]GUIDRecord, error)
}

// MaildirStamp is what a folder's directories and uid list looked like when the
// index was written, in the reference's layout (maildir-storage.h:52-56).
type MaildirStamp struct {
	NewCheckTime, NewMtime, NewMtimeNsecs uint32
	CurCheckTime, CurMtime, CurMtimeNsecs uint32
	UIDListMtime, UIDListMtimeNsecs       uint32
	UIDListSize                           uint32
}

// MaildirStamped is an index that keeps that stamp, so opening a folder can ask
// one stat whether the uid list is the one the index was built from.
type MaildirStamped interface {
	MaildirStamp(folderID uint64) (MaildirStamp, bool)
	SetMaildirStamp(folderID uint64, s MaildirStamp) error
}

// VirtualBacking is one folder a virtual mailbox draws from. Keyed by GUID, so
// a rename keeps the mapping (#1995); the name is for reading, not matching.
type VirtualBacking struct {
	ID            uint32
	GUID          [16]byte
	Name          string
	UIDValidity   uint32
	NextUID       uint32
	HighestModSeq uint64
}

// VirtualHeader is what a virtual mailbox's index says about the set it holds
// (the reference's virtual index header, virtual-storage.h:20-45).
type VirtualHeader struct {
	// ChangeCounter rises with each write, so a reader can tell the header it
	// read from one written since.
	ChangeCounter uint32
	// HighestBackingID is never reused: a record written before a folder left
	// must not read as one that joined afterwards.
	HighestBackingID uint32
	// SearchCRC32 covers the configuration's rules; a different value means
	// the set is defined differently, so the mailbox is rebuilt, not extended.
	SearchCRC32 uint32
	Backing     []VirtualBacking
}

// AssignBacking returns the id this folder already holds, or the next one.
func (h *VirtualHeader) AssignBacking(guid [16]byte, name string, uidValidity uint32) uint32 {
	for i := range h.Backing {
		if h.Backing[i].GUID == guid {
			h.Backing[i].Name = name
			h.Backing[i].UIDValidity = uidValidity
			return h.Backing[i].ID
		}
	}
	h.HighestBackingID++
	h.Backing = append(h.Backing, VirtualBacking{
		ID: h.HighestBackingID, GUID: guid, Name: name, UIDValidity: uidValidity,
	})
	return h.HighestBackingID
}

// NeedsRebuild says the configuration no longer describes what was indexed.
func (h *VirtualHeader) NeedsRebuild(crc uint32) bool { return h.SearchCRC32 != crc }

// VirtualIndexed is an index that keeps a virtual mailbox's own header.
type VirtualIndexed interface {
	VirtualHeader(folderID uint64) (VirtualHeader, bool)
	SetVirtualHeader(folderID uint64, h VirtualHeader) error
}

// UIDAddressable is a driver that finds a message from the record itself: the
// name is derived from what the folder records, not kept beside it (#1700).
type UIDAddressable interface {
	// OpenRecord returns the message body. Caller closes.
	OpenRecord(folder string, m *MessageMeta) (io.ReadCloser, error)
	// RecordPath names the message for the operations that take a name --
	// copy, move, remove -- which stay the driver's own vocabulary.
	RecordPath(folder string, m *MessageMeta) (string, error)
}

// StorageKeyer is a driver whose messages have a storage key of their own: the
// mdbox map_uid, which the mailbox index carries per record as the reference does.
type StorageKeyer interface {
	// StorageKey reads the key out of what Save returned for this message.
	StorageKey(folder, filename string) (mapUID, saveDate uint32, ok bool)
}

// UIDNamer is a driver whose file name is the uid: the name cannot be settled
// until the uid is, so a save leaves a temp file and this renames it.
type UIDNamer interface {
	// AssignUID renames the file a save left under tempName to the name uid
	// gives it, and returns that name. Called inside the caller's own cycle.
	AssignUID(folder, tempName string, uid uint32) (string, error)
}

// NamingAppender allocates a uid and settles the message's name in one cycle:
// two cycles would take the folder key twice for one APPEND.
type NamingAppender interface {
	// AllocateAndAppendNamed assigns the uid, calls name with it, records the
	// answer as the message's filename, and appends -- all under one hold.
	AllocateAndAppendNamed(folderID uint64, m *MessageMeta, name func(uid uint32) (string, error)) error
}

// StaleTemp is when a body a save left behind stops meaning "in flight" and
// starts meaning "died": one value, so every driver sweeps alike (#1736).
const StaleTemp = 24 * time.Hour

// FolderRefresher re-reads a folder from disk. A sync that imports what its own
// snapshot does not hold must decide on the folder, not on its snapshot (#1739).
type FolderRefresher interface {
	RefreshFolder(folderID uint64) error
}

// RecordSizer answers a message's sizes from where its driver keeps them: a
// maildir name carries both, a dbox record already holds them (#1726).
type RecordSizer interface {
	RecordSize(folder string, m *MessageMeta) (size, vsize uint32, err error)
}

// SizelessLister names the records carrying no virtual size, so the driver that
// keeps the size elsewhere can fill them (#1728).
type SizelessLister interface {
	SizelessUIDs(folderID uint64) ([]uint32, error)
}

// SizeStamper writes a virtual size into records that carry none, for the pass
// that recovered the names those sizes were kept beside (#1728).
type SizeStamper interface {
	StampSizes(folderID uint64, vsizes map[uint32]uint32) (int, error)
}

// RecordTail is the half of a record that follows its storage key: what an
// operator repair rebuilds from the message itself.
type RecordTail struct {
	MapUID   uint32
	SaveDate uint32
	VSize    uint32
	GUID     [16]byte
}

// TailRepairer overwrites those fields for the named records. Unlike a stamp it
// replaces what is there, so only an explicit operator pass calls it (#1770).
type TailRepairer interface {
	RepairRecordTails(folderID uint64, tails map[uint32]RecordTail) (int, error)
}

// FlagsDirtyMarker records that a message's flags have not reached storage. A
// driver that keeps flags in the file name sets it when the rename fails.
type FlagsDirtyMarker interface {
	SetFlagsDirty(folderID uint64, uid uint32, dirty bool) error
}

type CorruptionMarker interface {
	// MarkFolderCorrupt sets the marker. Idempotent.
	MarkFolderCorrupt(folderID uint64) error
	// ClearFolderCorrupt clears the marker, under the mailbox lock after the heal
	// and by the operator rebuild endpoint.
	ClearFolderCorrupt(folderID uint64) error
}

// ExpungedCopy names a record a repair dropped: the uid it held and the
// message it was, so a search index can retract the right document (#1986).
type ExpungedCopy struct {
	UID  uint32
	GUID [16]byte
}

// ReactiveHealer is a driver that can self-heal a folder whose index references
// missing/corrupt storage. Only such drivers may have a folder flagged corrupt;
// a driver that cannot heal would strand the folder with nothing to clear the marker.
type ReactiveHealer interface {
	// HealCorruptFolder repairs folder and returns the UIDs it expunged (records
	// whose backing message vanished), so the caller can invalidate their FTS
	// documents.
	HealCorruptFolder(box Box, idx UserIndex, folder *Folder) ([]ExpungedCopy, error)
}

// CanReactiveHeal reports whether box can self-heal corruption. Marking a folder
// corrupt must be gated on this: flag only what something will later clear.
func CanReactiveHeal(box any) bool {
	if um, ok := box.(UserMailbox); ok {
		box = Driver(um)
	}
	_, ok := box.(ReactiveHealer)
	return ok
}

// FolderAgnosticStorage marks a driver whose Scan enumerates storage-wide (not
// per-folder) records, such as mdbox where messages are shared across folders. A
// per-folder rebuild is unsafe for such a driver — it would import every stored
// message into the target folder with fresh UIDs — so it needs a dedicated
// storage-wide rebuild and the per-folder endpoint must reject it.
type FolderAgnosticStorage interface {
	FolderAgnosticScan() bool
}

// StorageRebuildStats reports what a storage-wide rebuild did.
type StorageRebuildStats struct {
	Scanned             int    // physical messages read from storage
	FoldersRebuilt      int    // per-folder indexes reset
	Expunged            int    // map records dropped (message vanished from storage)
	UnreferencedZeroref int    // present messages referenced by no folder; refcount reset to 0 for purge (NOT resurrected)
	OrphansRestored     int    // unreferenced messages re-filed into their ORIG_MAILBOX (only when restoreOrphans is set)
	RebuildCount        uint32 // new generation counter after the rebuild
	FilesNormalised     int    // storage files rewritten to the header size they announce
	// ExpungedCopies maps folder name → records dropped from that folder's index by
	// the rebuild, so the operator caller can invalidate their FTS documents
	// (otherwise ghost entries until the next rescan).
	ExpungedCopies map[string][]ExpungedCopy
}

// StorageWideRebuilder is a folder-agnostic driver that can rebuild its whole
// storage under the storage (map) lock: reconcile the shared map against physical
// files, reset every folder index against surviving messages, recompute each map
// record's refcount from actual folder references (unreferenced messages become
// zero-ref garbage for the next purge, never resurrected), and drop map records
// whose message vanished. idx is the user's index backend for the same namespace.
//
// Orphan restore (re-filing an unreferenced message) happens only when
// restoreOrphans is set AND the message carries an ORIG_MAILBOX tag; it is re-filed
// into its recorded home folder, never blindly adopted. The default leaves
// unreferenced messages zero-ref for purge.
type StorageWideRebuilder interface {
	RebuildStorage(box Box, idx UserIndex, restoreOrphans bool) (StorageRebuildStats, error)
}

// FormatObjectID renders a 16-byte GUID as the RFC 8474 object identifier used
// for IMAP MAILBOXID / EMAILID (OBJECTID): 32 lowercase hex characters.
func FormatObjectID(guid [16]byte) string {
	return hex.EncodeToString(guid[:])
}

// MessageMeta holds per-message metadata stored in the index.
type MessageMeta struct {
	UID uint32
	// MapUID and SaveDate are the mdbox storage key, carried in the record so
	// the name is derived rather than stored beside it (#1700).
	MapUID   uint32
	SaveDate uint32
	// VirtualBacking and VirtualRealUID name where a virtual mailbox's message
	// really is: the backing folder's id, and its uid in that folder.
	VirtualBacking uint32
	VirtualRealUID uint32
	// FlagsDirty says the flags in the record have not reached storage yet, so
	// what the store says about them is older than what the client was told.
	FlagsDirty   bool
	Flags        []string
	Keywords     []string
	ModSeq       uint64
	Size         uint32
	VSize        uint32
	InternalDate time.Time
	GUID         [16]byte
	CacheOffset  uint32
	// CacheCRC is the checksum of the cache record at CacheOffset. Zero means
	// the index carries none -- an index another implementation wrote.
	CacheCRC uint32
	// AltTier is true when the message body resides in alt (cold) storage.
	// Stored as FlagBackend (0x40) in the on-disk index record so Fetch() opens
	// the correct tier without a wasted primary-tier syscall. Only meaningful for
	// mdbox; other drivers ignore it.
	AltTier bool
}

// Folder holds index-level folder metadata.
type Folder struct {
	ID            uint64
	Name          string
	UIDValidity   uint32
	NextUID       uint32
	Messages      uint32
	Unseen        uint32
	HighestModSeq uint64
	// GUID is a stable 16-byte identifier stamped at folder creation time.
	// Survives RENAME (unlike Name). Used as the key namespace for per-folder
	// metadata in pkg/dict (RFC 5464 METADATA) and as the rename-stable handle
	// for ACL state, quota counters, etc.
	GUID [16]byte
	// Fsckd is true when the folder index carries the persisted corruption marker:
	// a driver detected a missing/corrupt message and flagged the index for a
	// rebuild on the next open. The session runs the rebuild and clears the marker.
	Fsckd bool
}

// SeqSet is a set of UIDs or sequence numbers (use UID=0 for seq).
type SeqSet []SeqRange

type SeqRange struct {
	From, To uint32 // inclusive; To==0 means '*'
}

// FolderStamp is what a cheap validity check compares. Both files are included
// because a write moves the log and a fold moves both; the SIZE is what makes
// it sound where mtime granularity is coarse, as it is on NFS.
type FolderStamp struct {
	BaseSize, LogSize int64
	BaseMod, LogMod   time.Time
}

// FlagsUpdate carries the new flag and keyword sets for one message in a
// batch, as one transaction.
type FlagsUpdate struct {
	Flags    []string
	Keywords []string
	// Mode says what Flags and Keywords mean. The zero value replaces the whole
	// set, which is what STORE FLAGS (...) asks for. FlagsAdd and FlagsRemove
	// name only what changes, and the index resolves them against the record as
	// its lock finds it — the difference that keeps a concurrent change from
	// being overwritten by a set the caller read earlier (#1250).
	Mode FlagsMode
}

// FlagsMode selects how a FlagsUpdate is applied.
type FlagsMode int

const (
	// FlagsSet replaces the flag and keyword set. Correct where the caller
	// genuinely declares the set: STORE FLAGS (...), a Sieve setflag action, or
	// a maildir sync where the filename is the truth.
	FlagsSet FlagsMode = iota
	// FlagsAdd unions the named flags and keywords into the record's own.
	FlagsAdd
	// FlagsRemove subtracts them from the record's own.
	FlagsRemove
)

// FlagsResult is what a message ended up with after a batch update: the flags
// and keywords the index holds, and the modseq of the change. The resulting set
// is returned rather than assumed because under FlagsAdd or FlagsRemove the
// caller does not know it — that is the point of naming only the delta.
type FlagsResult struct {
	ModSeq   uint64
	Flags    []string
	Keywords []string
}

// SyncStats reports what a proactive index reconcile changed. Imported counts
// files that gained a UID, Expunged counts index records whose file vanished,
// Updated counts tracked messages whose on-disk filename was repointed. Changed
// is false when the folder matched the index exactly and no index write ran.
type SyncStats struct {
	Imported int
	Expunged int
	Updated  int
	// Relinked counts records whose list row was written back rather than
	// their message being imported a second time (#1785).
	Relinked int
	Changed  bool
}

// RFC822Size returns the size to report as RFC822.SIZE and to match in SEARCH
// LARGER/SMALLER: the VIRTUAL size, the octet count of the message as transmitted
// with CRLF line endings (RFC 3501 §6.4.5), never the physical on-disk size. When
// VSize == 0 the backend recorded no virtual size and the physical Size is used.
func (m *MessageMeta) RFC822Size() uint32 {
	if m == nil {
		return 0
	}
	if m.VSize != 0 {
		return m.VSize
	}
	return m.Size
}

// RFC822Size mirrors MessageMeta.RFC822Size for scan records.
func (r *ScanRecord) RFC822Size() uint32 {
	if r == nil {
		return 0
	}
	if r.VSize != 0 {
		return r.VSize
	}
	return r.Size
}

// ScanRecord is one entry produced by UserMailbox.Scan: the raw per-message info
// a driver reconstructs from disk alone (no index). Drives admin rebuild flows.
type ScanRecord struct {
	Filename     string
	GUID         [16]byte
	Size         uint32
	VSize        uint32
	InternalDate time.Time
	// Flags holds only system flags (backslash-prefixed). Keywords live in
	// Keywords and never in here: a merged list reads as a flag set to every
	// consumer that forwards it on, and one that did dropped every keyword an
	// adopted store held (#1605).
	Flags    []string
	Keywords []string
	// OrigMailbox is the mailbox a message was originally saved into, recovered
	// from storage metadata (mdbox trailer). Empty when unrecorded. A storage-wide
	// rebuild uses it to restore an orphan to its home folder instead of guessing.
	OrigMailbox string
}

// MailboxBackend is the per-process factory for user-scoped storage handles.
// It holds no per-user state; all per-user state lives in UserMailbox.
type MailboxBackend interface {
	OpenUser(*UserInfo) UserMailbox
}

// UserMailbox is a per-session, per-user storage handle bound to a single UserInfo
// at creation time. Init MUST be called before any other method; it creates the
// on-disk directory structure.
//
// Save takes the assigned UID as a parameter and returns the message's stored
// filename plus its VIRTUAL (CRLF-normalized) size, which must land in
// MessageMeta.VSize so RFC822.SIZE is consistent from the first FETCH. If Save
// fails after the UID was allocated, the UID is burnt and the index skips the hole
// on the next scan. Close releases any open file descriptors.
//
// Save is the single generation site for per-message GUIDs (RFC 8474 EMAILID,
// JMAP Email.id). A zero guid means "assign a fresh one"; a non-zero guid is
// stored verbatim so MOVE keeps one identity across folders. The returned GUID
// is the effective one and must land in MessageMeta.GUID. Drivers persist it in
// storage (dbox record trailer, maildir uidlist), so it survives an index
// rebuild.
type UserMailbox interface {
	// Username is whose mail this handle holds; every diagnostic line names it.
	Username() string
	Init() error
	Create(folder string) error
	Delete(folder string) error
	Rename(oldName, newName string) error
	// flags holds only system flags and keywords only keywords, the way a
	// record keeps them (#1605). A driver keeping flags in the name keeps both.
	Save(folder string, r io.Reader, uid uint32, size int64, flags, keywords []string, guid [16]byte) (name string, vsize uint32, outGUID [16]byte, err error)
	// Move relocates one message between folders keeping its identity: the
	// returned GUID equals guid (RFC 8474: MOVE must not change EMAILID).
	// Source and destination lock in name order, so a concurrent A->B / B->A
	// pair cannot deadlock.
	Move(srcFolder, dstFolder, filename string, guid [16]byte) (newName string, outGUID [16]byte, err error)
	// Fetch returns a reader for the message body. altTier hints that the
	// message lives in alt (cold) storage so the driver can open it directly
	// without trying the primary path first. The hint is set from
	// MessageMeta.AltTier which is persisted in the index as FlagBackend.
	Fetch(folder, filename string, altTier bool) (io.ReadCloser, error)
	Remove(folder, filename string) error
	List(folder string) ([]*MessageMeta, error)
	FolderExists(folder string) (bool, error)
	// ListFolders returns every folder in the user's personal namespace,
	// including nested folders (dbox drivers recurse the physical tree) and
	// \NoSelect containers (FolderEntry.Selectable=false).
	ListFolders() ([]FolderEntry, error)
	// Scan walks folder's on-disk representation and yields every visible message
	// as a ScanRecord, so the admin rebuild flow can regenerate the fileindex
	// independently of the index. Drivers without disk-scan return a "not yet
	// implemented" error.
	Scan(folder string) ([]ScanRecord, error)
	Close() error
}

// IndexBackend is the per-process factory for user-scoped index handles.
type IndexBackend interface {
	OpenUser(*UserInfo) UserIndex
}

// UserIndex is a per-session, per-user index handle.
// All folder IDs are local to this handle — they must not be shared across handles.
type UserIndex interface {
	OpenFolder(folder string, uidValidity uint32) (*Folder, error)
	// Begin opens a transaction on one folder: changes are queued against a
	// view and written once, under one hold (#1827).
	Begin(folderID uint64) (IndexTx, error)
	SaveFolder(f *Folder) error
	AppendMessage(folderID uint64, m *MessageMeta) error
	// AllocateUID atomically reserves and persists the folder's next UID under the
	// cross-process mailbox lock. If the caller fails between AllocateUID and
	// AppendMessage the UID is burnt (uid hole), reconciled by a later rebuild.
	AllocateUID(folderID uint64) (uint32, error)
	// AllocateUIDWithModSeq atomically reserves the folder's next UID and
	// pre-allocates the next modseq value in one lock/reload/flush cycle.
	AllocateUIDWithModSeq(folderID uint64) (uid uint32, modseq uint64, err error)
	// AllocateAndAppend assigns a UID and records the message in a single
	// lock/reload/flush cycle: m.UID and m.ModSeq are filled in by the call.
	AllocateAndAppend(folderID uint64, m *MessageMeta) error
	UpdateFlags(folderID uint64, uid uint32, flags, keywords []string) error

	// AddFlags adds flags and keywords, keeping whatever the message already
	// carries. Use it wherever the caller wants to set one flag rather than to
	// declare the whole set: UpdateFlags writes an absolute list, so a list
	// built from an earlier read silently drops every change made since (#1250).
	AddFlags(folderID uint64, uid uint32, flags, keywords []string) error

	// RemoveFlags clears flags and keywords, leaving the rest untouched. The
	// counterpart of AddFlags and needed for the same reason: clearing one flag
	// through UpdateFlags means sending the whole remaining set, which is a set
	// the caller read earlier.
	RemoveFlags(folderID uint64, uid uint32, flags, keywords []string) error
	// SetAltTier sets or clears the AltTier marker (FlagBackend) for every record
	// whose storage key matches one of the supplied names, under the
	// folder's cross-process mailbox lock. Called by the altmove API after
	// relocating mdbox files so subsequent Fetch calls skip the primary-tier open.
	SetAltTier(folderID uint64, filenames []string, altTier bool) error
	GetMessages(folderID uint64, uids SeqSet) ([]*MessageMeta, error)
	ExpungeMessage(folderID uint64, uid uint32) error
	NextModSeq(folderID uint64) (uint64, error)
	// Vanished returns UIDs expunged from folderID with modseq strictly
	// greater than sinceModSeq. Drives QRESYNC (RFC 7162) SELECT and
	// UID FETCH (CHANGEDSINCE N VANISHED) responses.
	Vanished(folderID uint64, sinceModSeq uint64) ([]uint32, error)
	// VanishedGUIDs answers the same question as Vanished by message identity:
	// a protocol that addresses messages by GUID cannot report a UID, and the
	// message is gone, so its GUID can be read from nowhere else afterwards.
	// complete is false when a record in range cannot be named, and the caller
	// must degrade rather than report a shorter list as the whole truth.
	VanishedGUIDs(folderID uint64, sinceModSeq uint64) (guids [][16]byte, complete bool, err error)
	// FolderStamp describes the folder's files without opening it: the sizes
	// and modification times of the base index and its log. It is a proof of
	// unchangedness for a cached read, never a substitute for reading -- the
	// log is what moves modseq between folds, so the base alone would answer
	// with a stale marker.
	FolderStamp(folder string) (FolderStamp, error)
	// ExpungeFloor reports the modseq below which Vanished can no longer
	// answer, because compaction folded the log that held those records. Zero
	// means nothing has been folded away. A caller below the floor must
	// degrade rather than read an empty Vanished as "nothing was deleted".
	ExpungeFloor(folderID uint64) (uint64, error)
	Keywords(folderID uint64) ([]string, error)
	// RenameFolder renames oldName to newName in the index.
	// Called by IMAP RENAME immediately after UserMailbox.Rename succeeds.
	RenameFolder(oldName, newName string) error
	// DeleteFolder removes folder's index state.
	// Called by IMAP DELETE immediately after UserMailbox.Delete succeeds.
	DeleteFolder(folder string) error
	// GetPOP3UIDLs loads saved POP3 UIDLs for a folder (uid → uidl string).
	// Returns an empty map when no saved UIDLs exist yet.
	GetPOP3UIDLs(folderID uint64) (map[uint32]string, error)
	// SavePOP3UIDLs persists POP3 UIDLs so subsequent sessions use stable values.
	SavePOP3UIDLs(folderID uint64, uidls map[uint32]string) error
	// ResetFolder atomically replaces folderID's on-disk record set with the
	// supplied messages. Preserves UIDVALIDITY and the folder GUID; bumps NextUID
	// past max(records.UID). Each surviving record keeps its own ModSeq (a record
	// with none is stamped fresh) and HighestModSeq advances to the greatest one.
	// Returns UIDs present before the reset but absent after, so the caller can
	// invalidate their FTS documents. Caller holds the mailbox lock and has made a
	// .bak of the old base file.
	ResetFolder(folderID uint64, records []*MessageMeta) ([]ExpungedCopy, error)
	// OptimizeIndex compacts the .index.log overlay into the base .index file,
	// under the same write lock as a normal write. No-op nil when nothing to compact.
	OptimizeIndex(folderID uint64) error
	// FolderVSize returns the folder's cached aggregate virtual size and message
	// count (the hdr-vsize extension), which the count quota backend sums across a
	// user's folders.
	FolderVSize(folderID uint64) (bytes uint64, messages uint32, err error)
	// GUIDBackfillNeeded reports whether the folder still holds records written
	// before per-message GUIDs existed. Constant time: it reads a header state,
	// never the records, so a large folder does not pay a scan on every open.
	GUIDBackfillNeeded(folderID uint64) (bool, error)
	// SetGUIDs stamps GUIDs onto existing records and marks the folder done.
	// Values must come from storage (Scan), never be invented here, or the
	// index and the mail store would disagree on a message's identity. Records
	// that already carry a GUID keep it, so a resumed pass is a no-op for them.
	SetGUIDs(folderID uint64, guids map[uint32][16]byte) error
	// RecomputeVSize forces a rebuild of the folder's vsize aggregate from
	// records and persists it — the admin recovery path for a corrupted count.
	RecomputeVSize(folderID uint64) error
	Close() error
}

// UnlockedReader is the optional capability an index has when its on-disk files
// can prove their own freshness: a read whose answer only reaches a caller can
// then be served without the cross-process lock. An index without the property
// is simply an index without this method, so callers ask for it rather than
// require it.
//
// It is deliberately not part of UserIndex. Making it a required method would
// force every index implementation to have the property or to lie about it, and
// the whole point is that a file which cannot prove its state falls back to the
// locked path.
type UnlockedReader interface {
	GetMessagesUnlocked(folderID uint64, uids SeqSet) ([]*MessageMeta, error)
}

// CorruptIndexError says the folder's index cannot be read because what is on
// disk is not what this version writes -- a wrong major version, a truncated
// header. It names the folder because that is the one thing an operator needs
// and the one thing a generic error loses: the failure is per folder, the rest
// of the account keeps working, and without the name there is nothing to point
// a repair at.
type CorruptIndexError struct {
	Folder string
	Err    error
}

func (e *CorruptIndexError) Error() string {
	return "mailbox: index of folder " + e.Folder + " is unreadable: " + e.Err.Error()
}

func (e *CorruptIndexError) Unwrap() error { return e.Err }

// ErrIndexLost says a folder this server recorded as existing has no index of
// ours any more, and cannot be rebuilt where it was found. It is deliberately
// not an empty mailbox: a folder answering 0 EXISTS over mail that is still in
// storage is the answer nobody checks (#1608).
var ErrIndexLost = errors.New("folder index lost")

// ErrNoSpace is wrapped by a write the volume refused for room. The data is
// intact and the same write works once there is room (#1831).
var ErrNoSpace = errors.New("mailbox: no space on the volume")

// NoSpaceError names the folder whose write the volume refused: the failure is
// per folder and the name is what an operator acts on (#1831).
type NoSpaceError struct {
	Folder string
	Err    error
}

func (e *NoSpaceError) Error() string {
	return "mailbox: no space for folder " + e.Folder + ": " + e.Err.Error()
}

func (e *NoSpaceError) Unwrap() []error { return []error{ErrNoSpace, e.Err} }

// DriverNamer names the driver a handle belongs to, so a counter can carry it
// without asking what concrete type is underneath.
type DriverNamer interface {
	DriverName() string
}

// DriverNameOf is the label for a box, or "other" for a driver that names none.
func DriverNameOf(box UserMailbox) string {
	if n, ok := Driver(box).(DriverNamer); ok {
		return n.DriverName()
	}
	return "other"
}

// CacheStamp is where a message's cache record starts and what it hashed to:
// a record that does not hash to it is not this message's (#1714).
type CacheStamp struct {
	Offset uint32
	CRC    uint32
}

// FolderCreator is an index that can be told a folder is being created rather
// than opened. The two differ in what a missing index means, and one call site
// answering both served the wrong one.
type FolderCreator interface {
	CreateFolder(folder string, uidValidity uint32) (*Folder, error)
}

// ForeignNameAdopter brings a store written by another implementation to this
// deployment's folder-name encoding. Called when the store is opened, before
// anything lists it: a listing served from their encoding shows names a client
// cannot select (#1609).
type ForeignNameAdopter interface {
	AdoptForeignNames() error
}

// FlagWrite is one message's settled flag set, for a driver that records flags
// outside the index.
type FlagWrite struct {
	UID      uint32
	Filename string
	Flags    []string
	Keywords []string
}

// FlagWriteResult is what the store holds for one message afterwards: the name
// it is under now, and the error that stopped it, if one did.
type FlagWriteResult struct {
	UID      uint32
	Filename string
	Err      error
}

// FlagWriterMulti records a whole command's flag writes at once.
//
// Separate from FlagWriter rather than replacing it: dbox keeps flags in the
// index and implements neither, and a caller falls back to the single-message
// form. The batch exists because the single one takes the folder's
// cross-process lock per message, so one STORE over 200 messages took the lock
// 200 times and every acquisition paid the BUSY retries of a contended folder
// (#1623).
type FlagWriterMulti interface {
	WriteFlagsMulti(folder string, writes []FlagWrite) []FlagWriteResult
}

// TempSweeper removes bodies a save never published, on a session open and
// behind its own interval gate (#1801).
type TempSweeper interface {
	SweepTemps(folder string)
}

// FolderHolder runs fn under the driver's own folder hold. The lock belongs to
// the storage, not to the protocol that opened it (#1794).
type FolderHolder interface {
	HoldFolder(folder, site string, fn func() error) error
}

// HeldRemover unlinks a body whose folder the caller already holds, for a
// driver whose Remove takes that hold itself and cannot be nested.
type HeldRemover interface {
	RemoveHeld(folder, filename string) error
}

// BatchRemover unlinks several bodies as one operation. A driver whose removal
// takes a user-wide lock pays for it once instead of once per message (#1884).
type BatchRemover interface {
	RemoveManyHeld(folder string, filenames []string) error
}

// SplitStoredFlags sorts one stored list back into the two fields MessageMeta
// keeps apart (#1605).
func SplitStoredFlags(all []string) (flags, keywords []string) {
	for _, f := range all {
		if strings.HasPrefix(f, `\`) {
			flags = append(flags, f)
		} else {
			keywords = append(keywords, f)
		}
	}
	return flags, keywords
}
