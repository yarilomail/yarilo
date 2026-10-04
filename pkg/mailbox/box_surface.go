package mailbox

import (
	"errors"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-message/textproto"

	"github.com/yarilomail/yarilo/pkg/locks"
)

// BoxTx is one command's changes to one folder, written once. A change to a
// record it read applies only while that record is as read (#1805).
type BoxTx interface {
	// Messages reads records, remembering each one's modseq for the changes.
	Messages(set SeqSet) ([]*MessageMeta, error)
	UpdateFlags(uid uint32, upd FlagsUpdate)
	Expunge(uid uint32)
	Append(m *MessageMeta)
	// Commit writes what is still true; a change to a record that moved or
	// went is skipped and named in TxResult.Skipped, the rest is written.
	Commit() (TxResult, error)
	Rollback()
}

// FolderMetadata is what a folder's index says about its contents.
type FolderMetadata struct {
	VSize    uint64
	Messages uint32
}

// EnvelopeHead is the part of an envelope that SORT and THREAD compare.
type EnvelopeHead struct {
	Date    time.Time
	Subject string
	// From, To and Cc carry the first address's mailbox part, the RFC 5256
	// sort key.
	From, To, Cc string
	InReplyTo    []string
	MessageID    string
}

// EnvelopeCacheOptions is who opens a folder's envelope cache and how.
type EnvelopeCacheOptions struct {
	Locker    locks.Locker
	User      string
	SessionID string
	Folder    string
	TraceID   string
	// DeferWrites drops the cache locks once the file is read and takes them
	// again on Close; Shared lets readers of one folder share them.
	DeferWrites bool
	Shared      bool
}

// EnvelopeCache is one folder's parsed-envelope cache, as a protocol uses it.
type EnvelopeCache interface {
	Preload()
	Head(m *MessageMeta) (EnvelopeHead, bool)
	HeadAndReferences(m *MessageMeta) (EnvelopeHead, []string, bool)
	Envelope(m *MessageMeta) *imaplib.Envelope
	EnvelopeText(m *MessageMeta) (string, bool)
	BodyStructure(m *MessageMeta) imaplib.BodyStructure
	Sizes(m *MessageMeta) (size, vsize uint32, ok bool)
	StoreFromHeader(m *MessageMeta, h textproto.Header, text string)
	StoreBodyStructure(m *MessageMeta, bs imaplib.BodyStructure)
	StoreReferences(m *MessageMeta, refs []string)
	StoreSentDate(m *MessageMeta, t time.Time)
	StoreSizes(m *MessageMeta, size, vsize uint32)
	Close()
}

// SelfSyncing is a store whose folders it derives itself, brought up to date
// with the index the base owns: open settles a pass, Poll asks for one.
type SelfSyncing interface {
	SyncFolder(idx UserIndex, f *Folder, open bool) (changed bool, err error)
}

// BackingRef is the folder of the personal namespace a virtual record's copy
// lives in, by the id the record names it with.
type BackingRef struct {
	Name        string
	GUID        [16]byte
	UIDValidity uint32
}

// VirtualCopies says where a derived folder's copies live; asserted where it
// is used, since only a virtual mailbox has copies.
type VirtualCopies interface {
	Backing(folderID uint64) (map[uint32]BackingRef, error)
}

// ErrNoGUIDStore: the account keeps no record of copies by message, so a
// caller finds them another way.
var ErrNoGUIDStore = errors.New("mailbox: no record of copies by message")
